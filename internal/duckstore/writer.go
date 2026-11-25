package duckstore

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	duckdb "github.com/duckdb/duckdb-go/v2"

	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/schema"
)

// write stores one batch: each signal's records, with their tallies, in one
// transaction. A file that fails sends the batch, and the rest of the
// session, to memory.
func (e *Engine) write(batch []record.Record) {
	defer e.archiveAtMidnight(batch)
	bySource := map[string][]record.Record{}
	var order []string
	for _, r := range batch {
		src := r.Key().Source
		if bySource[src] == nil {
			order = append(order, src)
		}
		bySource[src] = append(bySource[src], r)
	}
	for _, src := range order {
		recs := bySource[src]
		p := e.dest[src]
		err := e.writeTx(p, recs)
		if err == nil {
			continue
		}
		if !p.file {
			e.warnOnce("loghorn: DuckDB write failed: " + err.Error())
			continue
		}
		e.fileFailed(err)
		if err := e.writeTx(e.dest[src], recs); err != nil {
			e.warnOnce("loghorn: DuckDB write failed: " + err.Error())
		}
	}
	e.ownerOnly()
	e.version.Add(1)
}

// writeTx appends recs to p and adds their tallies, all or nothing.
func (e *Engine) writeTx(p *part, recs []record.Record) error {
	if p.file && e.failWrites {
		return errors.New("write refused (test)")
	}
	if err := e.exec("BEGIN TRANSACTION"); err != nil {
		return err
	}
	err := e.appendRows(p, recs)
	if err == nil {
		err = e.saveTallies(p, recs)
	}
	if err == nil {
		if err = e.exec("COMMIT"); err == nil {
			return nil
		}
	}
	e.exec("ROLLBACK") // fails harmlessly when DuckDB already aborted it
	return err
}

// fileFailed handles a failed file write: warn once, and send this and
// every later record to memory. The records stay in the text log.
func (e *Engine) fileFailed(err error) {
	if !e.failed {
		e.failed = true
		e.warn(fmt.Sprintf("loghorn: DuckDB write failed (%v); keeping this run's later records in memory (the text log keeps them)", firstLine(err)))
	}
	for src, p := range e.mem {
		e.dest[src] = p
	}
}

func (e *Engine) warnOnce(msg string) {
	key := firstLine(fmt.Errorf("%s", msg))
	if len(key) > 60 {
		key = key[:60]
	}
	if e.warned[key] {
		return
	}
	e.warned[key] = true
	e.warn(msg)
}

func firstLine(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// rebuild turns a stored _raw back into its record's fields, the same way
// the record was first built.
func (e *Engine) rebuild(source string, raw []byte) (record.Record, bool) {
	if source == record.SourceOtel {
		return record.NormalizeSpan(raw)
	}
	return record.Normalize(e.parser.ParseLine(raw), e.in)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// row is a record's values in column order.
func row(r record.Record) []driver.Value {
	var tm any
	if !r.Time.IsZero() {
		tm = r.Time.UTC()
	}
	src := r.Key().Source
	tid, sid := record.TraceRef(r.Fields)
	sev, _ := r.Fields["severity"].(string)
	var parent, msg, dur any
	if src == record.SourceOtel {
		p, _ := r.Fields["parent_span_id"].(string)
		parent = nullStr(record.NormalizeSpanID(p))
		n, _ := r.Fields["name"].(string)
		msg = nullStr(n)
		if d, ok := r.Fields["duration_ms"].(json.Number); ok {
			if f, err := d.Float64(); err == nil {
				dur = f
			}
		}
	} else {
		m, _ := r.Fields["message"].(string)
		msg = nullStr(m)
	}
	raw := r.Raw
	if raw == nil {
		raw = []byte{}
	}
	return []driver.Value{int64(r.Seq), tm, r.TimeInherited, src, r.Service, r.Stream,
		nullStr(tid), nullStr(sid), parent, nullStr(sev), msg, dur, json.RawMessage(attributes(r.Fields)), raw}
}

func (e *Engine) appendRows(p *part, recs []record.Record) error {
	return e.wc.Raw(func(dc any) error {
		a, err := duckdb.NewAppender(dc.(driver.Conn), p.alias, "main", tableFor(p.source))
		if err != nil {
			return err
		}
		for _, r := range recs {
			if err := a.AppendRow(row(r)...); err != nil {
				a.Close()
				return fmt.Errorf("append to %s: %w", p.ref(), err)
			}
		}
		return a.Close()
	})
}

type tallyKey struct {
	day                         int
	service, source, kind, path string
}

// saveTallies adds recs' field paths to p's catalog's tallies.
func (e *Engine) saveTallies(p *part, recs []record.Record) error {
	tl := map[tallyKey]*schema.Tally{}
	for _, r := range recs {
		for path, v := range r.Fields {
			k := tallyKey{dayOf(r.Seq), r.Service, r.Key().Source, r.Stream, path}
			t := tl[k]
			if t == nil {
				t = &schema.Tally{}
				tl[k] = t
			}
			t.Count++
			t.Kinds |= schema.Classify(v)
		}
	}
	keys := make([]tallyKey, 0, len(tl))
	for k := range tl {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].path < keys[j].path })
	for len(keys) > 0 {
		n := min(len(keys), 500)
		var sb strings.Builder
		args := make([]any, 0, n*7)
		sb.WriteString("INSERT INTO " + p.fieldsRef() + " VALUES ")
		for i, k := range keys[:n] {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("(?, ?, ?, ?, ?, ?, ?)")
			t := tl[k]
			args = append(args, k.day, k.service, k.source, k.kind, k.path, int64(t.Count), int32(t.Kinds))
		}
		sb.WriteString(" ON CONFLICT DO UPDATE SET n = n + excluded.n, kinds = kinds | excluded.kinds")
		if err := e.exec(sb.String(), args...); err != nil {
			return err
		}
		keys = keys[n:]
	}
	return nil
}

// applyFill sets the time of records written without one.
func (e *Engine) applyFill(f *fill) {
	ids := map[string][]string{}
	for _, r := range f.recs {
		src := r.Key().Source
		ids[src] = append(ids[src], strconv.Itoa(r.Seq))
	}
	ns := f.t.UnixNano()
	for _, p := range e.parts {
		if !p.writable || len(ids[p.source]) == 0 {
			continue
		}
		in := strings.Join(ids[p.source], ",")
		if err := e.exec("UPDATE "+p.ref()+" SET _time = make_timestamp_ns(CAST(? AS BIGINT)) WHERE _id IN ("+in+")", ns); err != nil {
			e.warnOnce("loghorn: DuckDB: " + err.Error())
		}
	}
	e.version.Add(1)
}

func (e *Engine) exec(q string, args ...any) error {
	_, err := e.wc.ExecContext(bg, q, args...)
	if err != nil {
		return fmt.Errorf("%w\n%s", err, q)
	}
	return nil
}
