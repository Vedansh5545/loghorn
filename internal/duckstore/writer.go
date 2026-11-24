package duckstore

import (
	"database/sql/driver"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	duckdb "github.com/duckdb/duckdb-go/v2"

	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/schema"
)

// write stores one batch. A batch crossing midnight is written in parts,
// each day's records before the rollover that makes its file read-only.
func (e *Engine) write(batch []record.Record) {
	for len(batch) > 0 {
		n := e.beforeRollover(batch)
		e.writePart(batch[:n])
		batch = batch[n:]
	}
}

// beforeRollover is how many records lead batch before one that rolls the
// writer over to a later day (at least 1).
func (e *Engine) beforeRollover(batch []record.Record) int {
	if e.days == nil || !e.days.Write || e.today == nil {
		return len(batch)
	}
	cur := e.today.logs.day
	for i, r := range batch {
		if d := dayOf(r.Seq); d > cur {
			if i > 0 {
				return i
			}
			cur = d
		}
	}
	return len(batch)
}

// writePart stores records grouped by day pair and signal. A file catalog
// that fails sends the rest of the session to memory (onFail).
func (e *Engine) writePart(batch []record.Record) {
	defer e.unlockSchema()
	type group struct {
		c    *catalog
		recs []record.Record
	}
	var groups []*group
	byCat := map[*catalog]*group{}
	for _, r := range batch {
		p, err := e.pairFor(dayOf(r.Seq))
		if err != nil || p == nil {
			e.fail(err)
			if p, err = e.memFallback(); err != nil {
				continue
			}
		}
		c := p.cat(r.Key().Source)
		g := byCat[c]
		if g == nil {
			g = &group{c: c}
			byCat[c] = g
			groups = append(groups, g)
		}
		g.recs = append(g.recs, r)
	}
	changed := false
	for _, g := range groups {
		ch, err := e.writeCatalogTx(g.c, g.recs)
		changed = changed || ch
		if err == nil {
			ownerOnlyWAL(g.c)
			continue
		}
		if g.c.day < 0 { // memory itself failed: nothing left to try
			e.warnOnce("loghorn: DuckDB write failed: " + err.Error())
			continue
		}
		e.fail(err)
		e.dropIfGone(g.c)
		changed = true
		m, merr := e.memFallback()
		if merr != nil {
			continue
		}
		ch, err = e.writeCatalog(m.cat(g.c.source), g.recs)
		changed = changed || ch
		if err != nil {
			e.warnOnce("loghorn: DuckDB write failed: " + err.Error())
		}
	}
	if changed {
		if err := e.rebuildViews(); err != nil {
			e.warnOnce("loghorn: DuckDB views: " + err.Error())
		}
	}
	e.version.Add(1)
}

// ownerOnlyWAL makes a file catalog's write-ahead log 0600 like the file
// itself. DuckDB creates the WAL at the first write after an attach or a
// checkpoint, with the process umask, so this runs after every batch.
func ownerOnlyWAL(c *catalog) {
	if c.path == "" || !c.writable || c.day < 0 {
		return
	}
	wal := c.path + ".wal"
	if fi, err := os.Stat(wal); err == nil && fi.Mode().Perm() != 0o600 {
		os.Chmod(wal, 0o600)
	}
}

func (e *Engine) warnOnce(msg string) {
	key := msg
	if i := strings.IndexByte(key, '\n'); i >= 0 {
		key = key[:i]
	}
	if len(key) > 60 {
		key = key[:60]
	}
	if e.warned[key] {
		return
	}
	e.warned[key] = true
	e.warn(msg)
}

// fail reports a file write failure once and lets the day owner mark it.
func (e *Engine) fail(err error) {
	if err == nil {
		return
	}
	if e.onFail != nil {
		e.onFail(err)
	}
}

// dropIfGone forgets a catalog that is no longer attached, so views stop
// reading it.
func (e *Engine) dropIfGone(c *catalog) {
	var n int
	if err := e.wc.QueryRowContext(bg, "SELECT count(*) FROM duckdb_databases() WHERE database_name = ?", c.alias).Scan(&n); err == nil && n == 0 {
		for i, x := range e.cats {
			if x == c {
				e.cats = append(e.cats[:i:i], e.cats[i+1:]...)
				break
			}
		}
		if e.today != nil && (e.today.logs == c || e.today.traces == c) {
			e.today = nil
		}
	}
}

// memFallback returns the in-memory pair, creating it on first use, and
// routes every later record there.
func (e *Engine) memFallback() (*pair, error) {
	if e.mem == nil {
		m, err := e.memPair()
		if err != nil {
			e.warnOnce("loghorn: DuckDB: " + err.Error())
			return nil, err
		}
		e.mem = m
	}
	m := e.mem
	e.pairFor = func(int) (*pair, error) { return m, nil }
	return m, nil
}

// tableKey is the table a record goes to: its stream for a log, its span
// type for a span (D-003).
func tableKey(r record.Record) string {
	if r.Key().Source == record.SourceOtel {
		if s, _ := r.Fields["span.type"].(string); s != "" {
			return s
		}
		return record.StreamSpan
	}
	return r.Stream
}

// writeCatalogTx writes recs to a day file catalog in one transaction. If
// any step fails, nothing of the batch stays in the file and c's model is
// reloaded from the file, so the caller can send the whole batch to memory
// without showing part of it twice. Memory catalogs are written directly.
func (e *Engine) writeCatalogTx(c *catalog, recs []record.Record) (changed bool, err error) {
	if c.day < 0 {
		return e.writeCatalog(c, recs)
	}
	if err := e.exec("BEGIN TRANSACTION"); err != nil {
		return false, err
	}
	changed, err = e.writeCatalog(c, recs)
	if err == nil {
		if err = e.exec("COMMIT"); err == nil {
			return changed, nil
		}
	}
	e.exec("ROLLBACK") // fails harmlessly when DuckDB already aborted it
	e.reloadMeta(c)    // fails when c is gone; dropIfGone then forgets it
	return true, err
}

// reloadMeta replaces c's model of tables, columns and tallies with what
// its file holds.
func (e *Engine) reloadMeta(c *catalog) error {
	c.schemas = newNamer(schemaReserved...)
	c.tnames = map[string]*namer{}
	c.tables = map[tkey]*table{}
	c.order = nil
	c.tallies = map[fkey]*schema.Tally{}
	c.changed = map[fkey]bool{}
	return e.loadMeta(c)
}

// writeCatalog appends recs to c, creating tables and columns and changing
// column types first. It reports whether the schema changed.
func (e *Engine) writeCatalog(c *catalog, recs []record.Record) (changed bool, err error) {
	type group struct {
		t     *table
		recs  []record.Record
		paths map[string]bool
	}
	var groups []*group
	byTable := map[*table]*group{}
	for _, r := range recs {
		t, created, err := e.ensureTable(c, r.Service, tableKey(r))
		if err != nil {
			return changed, err
		}
		changed = changed || created
		g := byTable[t]
		if g == nil {
			g = &group{t: t, paths: map[string]bool{}}
			byTable[t] = g
			groups = append(groups, g)
		}
		g.recs = append(g.recs, r)
		for path, v := range r.Fields {
			k := schema.Classify(v)
			if t.kinds[path]|k != t.kinds[path] && schema.TallyType(path, 0) == schema.TTimestamp {
				changed = true // readers' frozen kinds decide epoch units (convCol)
			}
			t.kinds[path] |= k
			g.paths[path] = true
			fk := fkey{r.Service, t.key, r.Stream, path}
			tl := c.tallies[fk]
			if tl == nil {
				tl = &schema.Tally{}
				c.tallies[fk] = tl
			}
			tl.Count++
			tl.Kinds |= k
			c.changed[fk] = true
		}
	}
	for _, g := range groups {
		paths := make([]string, 0, len(g.paths))
		for p := range g.paths {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			want := schema.TallyType(p, g.t.kinds[p])
			col := g.t.byPath[p]
			switch {
			case col == nil:
				if err := e.addColumn(c, g.t, p, want); err != nil {
					return changed, err
				}
				changed = true
			case col.typ != want:
				if err := e.retype(c, g.t, col, want); err != nil {
					return changed, err
				}
				changed = true
			}
		}
		if err := e.appendRows(c, g.t, g.recs); err != nil {
			return changed, err
		}
	}
	return changed, e.saveTallies(c)
}

func (e *Engine) ensureTable(c *catalog, service, key string) (*table, bool, error) {
	if t := c.tables[tkey{service, key}]; t != nil {
		return t, false, nil
	}
	sch := c.schemas.name(service)
	tn := c.tnames[sch]
	if tn == nil {
		tn = newNamer()
		c.tnames[sch] = tn
	}
	t := &table{service: service, key: key, schema: sch, name: tn.name(key),
		byPath: map[string]*column{}, names: newNamer(columnReserved()...), kinds: map[string]schema.KindSet{}}
	cols := make([]string, len(sharedCols))
	for i, sc := range sharedCols {
		cols[i] = ident(sc.name) + " " + sc.typ
	}
	if err := e.exec("CREATE SCHEMA IF NOT EXISTS " + ident(c.alias) + "." + ident(sch)); err != nil {
		return nil, false, err
	}
	if err := e.exec("CREATE TABLE " + t.ref(c.alias) + " (" + strings.Join(cols, ", ") + ")"); err != nil {
		return nil, false, err
	}
	if err := e.exec("INSERT INTO "+c.meta("_tables")+" VALUES (?, ?, ?, ?)", service, key, t.schema, t.name); err != nil {
		return nil, false, err
	}
	c.addTable(t)
	return t, true, nil
}

func (e *Engine) addColumn(c *catalog, t *table, path string, typ schema.Type) error {
	col := &column{path: path, name: t.names.name(path), typ: typ}
	if err := e.exec("ALTER TABLE " + t.ref(c.alias) + " ADD COLUMN " + ident(col.name) + " " + sqlType(typ)); err != nil {
		return err
	}
	if err := e.exec("INSERT INTO "+c.meta("_columns")+" VALUES (?, ?, ?, ?, ?)", t.schema, t.name, path, col.name, string(typ)); err != nil {
		return err
	}
	t.cols = append(t.cols, col)
	t.byPath[path] = col
	return nil
}

// lockSchema keeps queries out until write publishes the new model (a
// type change, or catalogs detached at midnight). Writer only.
func (e *Engine) lockSchema() {
	if !e.schemaLocked {
		e.schemaMu.Lock()
		e.schemaLocked = true
	}
}

// unlockSchema lets queries in again once the model is published.
func (e *Engine) unlockSchema() {
	if e.schemaLocked {
		e.schemaLocked = false
		e.schemaMu.Unlock()
	}
}

// retype changes a column's type. A change to string rewrites every stored
// value from its record's _raw, so the column holds the text as received
// (as schema.Convert gives it); other changes (int to float, or a column of
// nulls getting its first values) are exact casts.
func (e *Engine) retype(c *catalog, t *table, col *column, typ schema.Type) error {
	e.lockSchema()
	ref, cn := t.ref(c.alias), ident(col.name)
	var rewrite []rewriteRow
	if typ == schema.TString {
		var err error
		if rewrite, err = e.rewriteValues(ref, col, typ); err != nil {
			return err
		}
	}
	if err := e.exec("ALTER TABLE " + ref + " ALTER " + cn + " SET DATA TYPE " + sqlType(typ) + " USING TRY_CAST(" + cn + " AS " + sqlType(typ) + ")"); err != nil {
		return err
	}
	if len(rewrite) > 0 {
		if err := e.exec("CREATE OR REPLACE TEMP TABLE lh_rewrite (id BIGINT, v VARCHAR)"); err != nil {
			return err
		}
		err := e.wc.Raw(func(dc any) error {
			a, err := duckdb.NewAppender(dc.(driver.Conn), "temp", "main", "lh_rewrite")
			if err != nil {
				return err
			}
			for _, r := range rewrite {
				if err := a.AppendRow(r.id, r.v); err != nil {
					a.Close()
					return err
				}
			}
			return a.Close()
		})
		if err != nil {
			return err
		}
		if err := e.exec("UPDATE " + ref + " SET " + cn + " = w.v, _bad = list_filter(_bad, lambda b: b <> " + lit(col.path) + ") FROM temp.main.lh_rewrite w WHERE " + ref + "._id = w.id"); err != nil {
			return err
		}
		if err := e.exec("DROP TABLE temp.main.lh_rewrite"); err != nil {
			return err
		}
	}
	col.typ = typ
	return e.exec("UPDATE "+c.meta("_columns")+" SET type = ? WHERE schema_name = ? AND table_name = ? AND path = ?", string(typ), t.schema, t.name, col.path)
}

type rewriteRow struct {
	id int64
	v  any
}

func (e *Engine) rewriteValues(ref string, col *column, typ schema.Type) ([]rewriteRow, error) {
	rows, err := e.wc.QueryContext(bg, "SELECT _id, _source, _raw FROM "+ref+" WHERE "+ident(col.name)+" IS NOT NULL OR coalesce(list_contains(_bad, "+lit(col.path)+"), false)")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []rewriteRow
	f := schema.Field{Path: col.path, Type: typ}
	for rows.Next() {
		var id int64
		var src, raw string
		if err := rows.Scan(&id, &src, &raw); err != nil {
			return nil, err
		}
		r, ok := e.rebuild(src, []byte(raw))
		if !ok {
			continue
		}
		out = append(out, rewriteRow{id, value(schema.Convert(r.Fields[col.path], f), typ)})
	}
	return out, rows.Err()
}

// rebuild turns a stored _raw back into its record's fields, the same way
// the record was first built.
func (e *Engine) rebuild(source string, raw []byte) (record.Record, bool) {
	if source == record.SourceOtel {
		return record.NormalizeSpan(raw)
	}
	return record.Normalize(e.parser.ParseLine(raw), e.in)
}

// value is what the appender stores for a converted value.
func value(v schema.Value, t schema.Type) any {
	if v.Empty || v.Bad {
		return nil
	}
	switch norm(t) {
	case schema.TInt:
		return v.Int
	case schema.TFloat, schema.TDuration:
		return v.Num
	case schema.TBool:
		return v.Bool
	case schema.TTimestamp:
		return v.Time.UTC()
	}
	return v.Str
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// shared is a record's shared column values, _id to _name.
func shared(r record.Record) []driver.Value {
	var tm any
	if !r.Time.IsZero() {
		tm = r.Time.UTC()
	}
	tid, sid := record.TraceRef(r.Fields)
	var parent, name any
	if r.Key().Source == record.SourceOtel {
		p, _ := r.Fields["parent_span_id"].(string)
		parent = nullStr(record.NormalizeSpanID(p))
		n, _ := r.Fields["name"].(string)
		name = nullStr(n)
	}
	return []driver.Value{int64(r.Seq), tm, r.TimeInherited, r.Key().Source, r.Service, r.Stream,
		nullStr(tid), nullStr(sid), parent, name}
}

func (e *Engine) appendRows(c *catalog, t *table, recs []record.Record) error {
	return e.wc.Raw(func(dc any) error {
		a, err := duckdb.NewAppender(dc.(driver.Conn), c.alias, t.schema, t.name)
		if err != nil {
			return err
		}
		row := make([]driver.Value, len(sharedCols)+len(t.cols))
		idx, err := duckdb.NewAppender(dc.(driver.Conn), c.alias, metaSchema, "_records")
		if err != nil {
			a.Close()
			return err
		}
		for _, r := range recs {
			var bad []string
			for i, col := range t.cols {
				v := schema.Convert(r.Fields[col.path], schema.Field{Path: col.path, Type: col.typ})
				if v.Bad {
					bad = append(bad, col.path)
				}
				row[len(sharedCols)+i] = value(v, col.typ)
			}
			var badv any
			if bad != nil {
				badv = bad
			}
			sh := shared(r)
			copy(row, sh)
			row[indexShared], row[indexShared+1] = badv, string(r.Raw)
			if err := a.AppendRow(row...); err != nil {
				a.Close()
				idx.Close()
				return fmt.Errorf("append to %s: %w", t.ref(c.alias), err)
			}
			if err := idx.AppendRow(append(sh, int32(t.tid))...); err != nil {
				a.Close()
				idx.Close()
				return fmt.Errorf("append to %s: %w", c.meta("_records"), err)
			}
		}
		if err := a.Close(); err != nil {
			idx.Close()
			return err
		}
		return idx.Close()
	})
}

// saveTallies writes the changed tallies to _fields.
func (e *Engine) saveTallies(c *catalog) error {
	if len(c.changed) == 0 {
		return nil
	}
	keys := make([]fkey, 0, len(c.changed))
	for k := range c.changed {
		keys = append(keys, k)
	}
	for len(keys) > 0 {
		n := min(len(keys), 500)
		var sb strings.Builder
		args := make([]any, 0, n*6)
		sb.WriteString("INSERT OR REPLACE INTO " + c.meta("_fields") + " VALUES ")
		for i, k := range keys[:n] {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("(?, ?, ?, ?, ?, ?)")
			tl := c.tallies[k]
			args = append(args, k.service, k.tkey, k.kind, k.path, int64(tl.Count), int32(tl.Kinds))
		}
		if err := e.exec(sb.String(), args...); err != nil {
			return err
		}
		keys = keys[n:]
	}
	clear(c.changed)
	return nil
}

// applyFill sets the time of records written without one.
func (e *Engine) applyFill(f *fill) {
	defer e.unlockSchema() // pairFor may roll over
	type place struct {
		c *catalog
		t *table
	}
	ids := map[place][]string{}
	var order []place
	for _, r := range f.recs {
		p, err := e.pairFor(dayOf(r.Seq))
		if err != nil || p == nil {
			continue
		}
		c := p.cat(r.Key().Source)
		t := c.tables[tkey{r.Service, tableKey(r)}]
		if t == nil {
			continue
		}
		pl := place{c, t}
		if ids[pl] == nil {
			order = append(order, pl)
		}
		ids[pl] = append(ids[pl], strconv.Itoa(r.Seq))
	}
	ns := f.t.UnixNano()
	for _, pl := range order {
		in := strings.Join(ids[pl], ",")
		for _, ref := range []string{pl.t.ref(pl.c.alias), pl.c.meta("_records")} {
			if err := e.exec("UPDATE "+ref+" SET _time = make_timestamp_ns(CAST(? AS BIGINT)) WHERE _id IN ("+in+")", ns); err != nil {
				e.warnOnce("loghorn: DuckDB: " + err.Error())
			}
		}
	}
	e.version.Add(1)
}
