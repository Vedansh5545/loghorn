// Package store is the session store behind the web query view: a façade
// over DuckDB (internal/duckstore). Add numbers records and fills in their
// service and time as before; reads go to SQL.
package store

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/jimbeveridge/loghorn/internal/adapter"
	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/duckstore"
	"github.com/jimbeveridge/loghorn/internal/ingest"
	"github.com/jimbeveridge/loghorn/internal/query"
	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/schema"
)

// Mode says where the session's records come from.
type Mode string

const (
	ModeLive       Mode = "live"
	ModeHistorical Mode = "historical"
	ModeGHJob      Mode = "gh-job"
)

// readWait is how long a read waits for records added before it to be
// written (D-015).
const readWait = 250 * time.Millisecond

// Info summarises the session.
type Info struct {
	Mode             Mode
	RunStart         time.Time
	Earliest, Latest time.Time
	Count            int
}

// StreamCount is a stream's name, source and record count. The same name
// under two sources is two streams.
type StreamCount struct {
	Name   string
	Source string
	Count  int
}

// Store holds the session's records.
type Store struct {
	e        *duckstore.Engine
	mode     Mode
	runStart time.Time
	now      func() time.Time // the arrival clock for live ids

	mu       sync.Mutex
	seq      map[int]int     // per day number: the last sequence used
	last     time.Time       // the previous record's time
	pending  []record.Record // leading records written with no time yet
	fallback string          // service of records that name none
	liveDay  int             // the latest arrival day given out

	cacheMu sync.Mutex
	cache   map[[3]string]cached
}

type cached struct {
	version uint64
	s       schema.Schema
}

// New returns an empty in-memory store. runStart is zero for replay modes.
// It panics if DuckDB cannot start, which only a broken build causes.
func New(mode Mode, runStart time.Time) *Store {
	return NewWithInput(mode, runStart, config.Input{})
}

// NewWithInput is New for records parsed with the input config in, which
// rebuilding a stored line needs.
func NewWithInput(mode Mode, runStart time.Time, in config.Input) *Store {
	e, err := duckstore.OpenMemory(in, func(msg string) { fmt.Fprintln(os.Stderr, msg) })
	if err != nil {
		panic(err)
	}
	return wrap(e, mode, runStart)
}

func wrap(e *duckstore.Engine, mode Mode, runStart time.Time) *Store {
	return &Store{e: e, mode: mode, runStart: runStart, now: time.Now, seq: map[int]int{},
		fallback: record.DefaultService, cache: map[[3]string]cached{}}
}

// SetFallbackService sets the service given to records added from now on that
// name none. Safe on a nil store.
func (s *Store) SetFallbackService(key string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.fallback = key
	s.mu.Unlock()
}

// DayNumber is the local date of t as days since 1970-01-01.
func DayNumber(t time.Time) int {
	y, m, d := t.Date()
	return int(time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400)
}

// Add numbers r and gives it its effective time. A zero r.Time takes
// received when non-zero, else the previous record's Time; with neither it
// is written with no time and given the next real time when one arrives
// (D-023). A live record's id carries its arrival day (D-011); a replay's
// day is 0.
func (s *Store) Add(r record.Record, received time.Time) {
	if s.mode == ModeLive {
		s.add(r, received, liveDay, false)
		return
	}
	s.add(r, received, 0, true)
}

// liveDay asks add for the arrival day, read under s.mu so days reach the
// writer in order: a day it has already closed would send the record to
// memory only.
const liveDay = -1

// add is Add with the id's day given; wait makes it wait for room in the
// writer's queue rather than skip (input with no text log behind it).
func (s *Store) add(r record.Record, received time.Time, day int, wait bool) {
	put := s.e.Add
	if wait {
		put = s.e.AddWait
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if day == liveDay {
		day = max(DayNumber(s.now()), s.liveDay) // never back: the clock can step
		s.liveDay = day
	}
	s.seq[day]++
	r.Seq = day<<32 + s.seq[day]
	if r.Service == "" {
		r.Service = s.fallback
	}
	if r.Time.IsZero() {
		r.TimeInherited = true
		switch {
		case !received.IsZero():
			r.Time = received
		default:
			r.Time = s.last
		}
	}
	if r.Time.IsZero() {
		s.pending = append(s.pending, r)
		put(r)
		return
	}
	if len(s.pending) > 0 {
		s.e.SetTime(s.pending, r.Time)
		s.pending = nil
	}
	s.last = r.Time
	put(r)
}

// Flush waits until everything added is written.
func (s *Store) Flush() { s.e.Sync(time.Hour) }

// Close flushes and closes the store.
func (s *Store) Close() error {
	s.Flush()
	return s.e.Close()
}

// read waits briefly for records added before it, then keeps the writer
// from changing catalogs (a type change, midnight) until unlock.
func (s *Store) read() (unlock func()) {
	s.e.Sync(readWait)
	return s.e.ReadLock()
}

// Records returns every record written so far, oldest first (for tests and
// tools); it waits for records already added to be written.
func (s *Store) Records() []record.Record {
	s.e.Sync(time.Hour)
	defer s.e.ReadLock()()
	recs, err := s.e.Records(context.Background(), "true")
	if err != nil {
		return nil
	}
	return recs
}

// Info returns the session summary. Earliest/Latest ignore zero times.
func (s *Store) Info() Info {
	defer s.read()()
	in := Info{Mode: s.mode, RunStart: s.runStart}
	var n int64
	var lo, hi *time.Time
	if err := s.e.DB().QueryRow("SELECT count(*), min(_time), max(_time) FROM all_records").Scan(&n, &lo, &hi); err == nil {
		in.Count = int(n)
		if lo != nil {
			in.Earliest, in.Latest = *lo, *hi
		}
	}
	return in
}

// Streams returns per-stream counts sorted by name, then source.
func (s *Store) Streams() []StreamCount {
	defer s.read()()
	out := []StreamCount{}
	rows, err := s.e.DB().Query("SELECT _stream, _source, count(*) FROM all_records GROUP BY 1, 2")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var c StreamCount
		if rows.Scan(&c.Name, &c.Source, &c.Count) == nil {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Source < out[j].Source
	})
	return out
}

// Preload reads stored log or span files into s, in order, with no received
// time, and waits until they are written.
func Preload(s *Store, paths []string, in config.Input) error {
	p := adapter.New(in)
	defer s.Flush()
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		err = ingest.Stored(f, func(rec []byte) {
			e := p.ParseLine(bytes.Clone(rec))
			if r, ok := record.Normalize(e, in); ok {
				s.Add(r, time.Time{})
			}
		})
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// Count is a name and how many records carry it.
type Count struct {
	Name  string
	Count int
}

// Kinds counts the records of source ("" is log) whose time is in [from,
// to). A zero bound is unbounded; a record with no time counts only when
// from is zero. services counts per service, ignoring service; kinds counts
// per Stream, limited to service unless it is "*". Both sort by count
// descending, then name, and are never nil.
func (s *Store) Kinds(source, service string, from, to time.Time) (kinds, services []Count) {
	kinds, services = []Count{}, []Count{}
	src, ok := record.ParseSource(source)
	if !ok {
		return
	}
	defer s.read()()
	cond := "_source = ?"
	args := []any{src}
	if !from.IsZero() {
		cond += " AND _time >= make_timestamp_ns(CAST(? AS BIGINT))"
		args = append(args, from.UnixNano())
	}
	if !to.IsZero() {
		cond += " AND (_time IS NULL OR _time < make_timestamp_ns(CAST(? AS BIGINT)))"
		args = append(args, to.UnixNano())
	}
	rows, err := s.e.DB().Query("SELECT _service, _stream, count(*) FROM all_records WHERE "+cond+" GROUP BY 1, 2", args...)
	if err != nil {
		return
	}
	defer rows.Close()
	kc, sc := map[string]int{}, map[string]int{}
	for rows.Next() {
		var svc, stream string
		var n int
		if rows.Scan(&svc, &stream, &n) != nil {
			continue
		}
		sc[svc] += n
		if service == "*" || svc == service {
			kc[stream] += n
		}
	}
	return sortedCounts(kc), sortedCounts(sc)
}

func sortedCounts(m map[string]int) []Count {
	out := make([]Count, 0, len(m))
	for n, c := range m {
		out = append(out, Count{Name: n, Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Schema is the inferred schema of a stream (record.AllStreams: every
// stream of source) within service ("" or "*": every service), exactly as
// schema.Infer gives it, cached until the data changes.
func (s *Store) Schema(service, source, stream string) (schema.Schema, error) {
	defer s.read()()
	k := [3]string{service, source, stream}
	v := s.e.Version()
	s.cacheMu.Lock()
	c, ok := s.cache[k]
	s.cacheMu.Unlock()
	if ok && c.version == v {
		return c.s, nil
	}
	src, ok := record.ParseSource(source)
	if !ok {
		src = source
	}
	tl, err := s.e.Tallies(src, service, stream)
	if err != nil {
		return schema.Schema{}, err
	}
	sch := schema.FromTallies(src, stream, tl)
	s.cacheMu.Lock()
	s.cache[k] = cached{v, sch}
	s.cacheMu.Unlock()
	return sch, nil
}

// Query answers q over the store with the effective schema sch.
func (s *Store) Query(ctx context.Context, sch schema.Schema, q query.Query, now time.Time) (query.Result, error) {
	defer s.read()()
	return query.RunSQL(ctx, s.e, sch, q, now, s.runStart)
}

// Record returns the record with id seq.
func (s *Store) Record(seq int) (record.Record, bool) {
	defer s.read()()
	recs, err := s.e.Records(context.Background(), "_id = "+strconv.Itoa(seq))
	if err != nil || len(recs) == 0 {
		return record.Record{}, false
	}
	return recs[0], true
}

// SpanRecords returns the records whose span id is spanID (normalized).
func (s *Store) SpanRecords(spanID string) []record.Record {
	id := record.NormalizeSpanID(spanID)
	if id == "" {
		return nil
	}
	defer s.read()()
	recs, _ := s.e.Records(context.Background(), "_span_id = ?", id)
	return recs
}

// TraceRecords returns the records of one trace (normalized trace id).
func (s *Store) TraceRecords(traceID string) []record.Record {
	id := record.NormalizeTraceID(traceID)
	if id == "" {
		return nil
	}
	defer s.read()()
	recs, _ := s.e.Records(context.Background(), "_trace_id = ?", id)
	return recs
}

// AggregateRecords returns what trace.Aggregate needs for operation name:
// the spans of every trace holding a span with that name, and the spans
// those spans name as parents.
func (s *Store) AggregateRecords(name string) []record.Record {
	defer s.read()()
	recs, _ := s.e.Records(context.Background(),
		"_source = 'otel' AND (_trace_id IN (SELECT _trace_id FROM all_records WHERE _source = 'otel' AND _name = ? AND _span_id IS NOT NULL AND _trace_id IS NOT NULL)"+
			" OR _span_id IN (SELECT _parent_span_id FROM all_records WHERE _source = 'otel' AND _name = ? AND _parent_span_id IS NOT NULL))", name, name)
	return recs
}
