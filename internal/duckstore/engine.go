// Package duckstore keeps loghorn's records in DuckDB: a database per day and
// signal (logs, traces), a schema per service, a table per stream and a
// typed column per field. Views in the session's own in-memory catalog join
// every attached day; the web view reads only those (D-004).
package duckstore

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	duckdb "github.com/duckdb/duckdb-go/v2"

	"github.com/jimbeveridge/loghorn/internal/adapter"
	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/record"
)

const (
	// MaxQueue bounds the records waiting to be written; past it, records
	// are skipped for DuckDB only and the day is marked dirty.
	MaxQueue = 200_000
	// maxBatch caps the records written in one batch.
	maxBatch = 20_000
)

// item is one queued write: a record, or a time to give records already
// queued or written.
type item struct {
	r    record.Record
	fill *fill
}

type fill struct {
	recs []record.Record
	t    time.Time
}

// Engine is one session's DuckDB instance and its writer.
type Engine struct {
	connector *duckdb.Connector
	db        *sql.DB
	wc        *sql.Conn // the writer's connection
	in        config.Input
	parser    *adapter.Parser // for in; stateless, so shared by rebuilds
	warn      func(string)

	mu        sync.Mutex
	cond      *sync.Cond
	queue     []item
	added     uint64 // records accepted by Add (queued or skipped)
	committed uint64 // records written or given up on
	closing   bool
	wdone     chan struct{}
	maxQueue  int          // MaxQueue, unless SetMaxQueue changed it
	overflow  bool         // the queue overflowed (warned once)
	skipped   map[int]bool // days with records skipped for DuckDB

	// Writer state: touched only by the writer goroutine, or before it
	// starts and after it stops.
	cats    []*catalog
	mem     *pair
	pairFor func(day int) (*pair, error)
	onFail  func(err error) // a file write failed (nil: memory only)
	vnames  *namer          // view names, stable across rebuilds
	wmu     sync.Mutex      // held while writing or changing catalogs
	// schemaMu keeps queries off a column whose type is changing: the
	// writer holds it from a type change until the new model is published.
	schemaMu     sync.RWMutex
	schemaLocked bool
	days         *DayOptions   // nil: memory only
	route        map[int]*pair // day -> writable pair
	today        *pair         // the open day (writer)
	failed       bool          // a file write failed: the rest goes to memory
	viewsMade    map[string]bool
	warned       map[string]bool // writer-side warnings already given

	snap    atomic.Pointer[model]
	version atomic.Uint64
}

// OpenMemory opens an engine holding everything in memory, for replays and
// tests. warn reports problems (once each) to the user.
func OpenMemory(in config.Input, warn func(string)) (*Engine, error) {
	e, err := open(in, warn)
	if err != nil {
		return nil, err
	}
	if e.mem, err = e.memPair(); err != nil {
		e.closeDB()
		return nil, err
	}
	e.pairFor = func(int) (*pair, error) { return e.mem, nil }
	if err := e.rebuildViews(); err != nil {
		e.closeDB()
		return nil, err
	}
	e.start()
	return e, nil
}

func open(in config.Input, warn func(string)) (*Engine, error) {
	c, err := duckdb.NewConnector("", nil)
	if err != nil {
		return nil, fmt.Errorf("duckdb: %w", err)
	}
	db := sql.OpenDB(c)
	wc, err := db.Conn(context.Background())
	if err != nil {
		db.Close()
		c.Close()
		return nil, fmt.Errorf("duckdb: %w", err)
	}
	if warn == nil {
		warn = func(string) {}
	}
	e := &Engine{connector: c, db: db, wc: wc, in: in, parser: adapter.New(in), warn: warn, vnames: newNamer(viewReserved...), viewsMade: map[string]bool{}, skipped: map[int]bool{}, warned: map[string]bool{}}
	e.cond = sync.NewCond(&e.mu)
	e.maxQueue = MaxQueue
	e.snap.Store(&model{})
	return e, nil
}

func (e *Engine) start() {
	e.wdone = make(chan struct{})
	go e.writer()
}

// memPair attaches the in-memory logs and traces catalogs.
func (e *Engine) memPair() (*pair, error) {
	l, err := e.attach("lmem", ":memory:", record.SourceLog, -1, true)
	if err != nil {
		return nil, err
	}
	t, err := e.attach("tmem", ":memory:", record.SourceOtel, -1, true)
	if err != nil {
		return nil, err
	}
	return &pair{logs: l, traces: t}, nil
}

// ReadLock keeps column types fixed while a query built from the current
// model runs; call the returned func when done.
func (e *Engine) ReadLock() (unlock func()) {
	e.schemaMu.RLock()
	return e.schemaMu.RUnlock
}

// DB is the session's database, for read queries.
func (e *Engine) DB() *sql.DB { return e.db }

// QueryContext runs a read query.
func (e *Engine) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return e.db.QueryContext(ctx, q, args...)
}

// Version changes whenever written data changes.
func (e *Engine) Version() uint64 { return e.version.Load() }

// Add queues r for writing; r.Seq is its _id. It never blocks on DuckDB:
// past the queue bound the record is skipped for DuckDB (live input, whose
// text log keeps it).
func (e *Engine) Add(r record.Record) { e.add(r, false) }

// AddWait is Add for input with no text log behind it (replays, loading
// text): when the queue is full it waits for room instead of skipping.
func (e *Engine) AddWait(r record.Record) { e.add(r, true) }

func (e *Engine) add(r record.Record, wait bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for wait && len(e.queue) >= e.maxQueue && !e.closing {
		e.cond.Wait()
	}
	e.added++
	if e.closing {
		e.committed++
		return
	}
	if len(e.queue) >= e.maxQueue {
		e.committed++
		if !e.overflow {
			e.overflow = true
			e.warn("loghorn: DuckDB writer is behind; skipping records for DuckDB (the text log keeps them)")
		}
		e.skipped[dayOf(r.Seq)] = true
		return
	}
	e.queue = append(e.queue, item{r: r})
	e.cond.Broadcast()
}

// SetMaxQueue changes the queue bound (for tests and tuning).
func (e *Engine) SetMaxQueue(n int) {
	e.mu.Lock()
	e.maxQueue = n
	e.mu.Unlock()
}

// SetTime gives recs, added earlier with no time, the time t. It is applied
// in order with the records queued around it.
func (e *Engine) SetTime(recs []record.Record, t time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.added++
	if e.closing {
		e.committed++
		return
	}
	e.queue = append(e.queue, item{fill: &fill{recs: append([]record.Record(nil), recs...), t: t}})
	e.cond.Broadcast()
}

// Sync waits until every record added before the call is written, or the
// timeout passes; it reports whether they were.
func (e *Engine) Sync(timeout time.Duration) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	target := e.added
	if e.committed >= target {
		return true
	}
	expired := false
	t := time.AfterFunc(timeout, func() {
		e.mu.Lock()
		expired = true
		e.cond.Broadcast()
		e.mu.Unlock()
	})
	defer t.Stop()
	for e.committed < target && !expired {
		e.cond.Wait()
	}
	return e.committed >= target
}

func (e *Engine) writer() {
	defer close(e.wdone)
	for {
		e.mu.Lock()
		for len(e.queue) == 0 && !e.closing {
			e.cond.Wait()
		}
		if len(e.queue) == 0 && e.closing {
			e.mu.Unlock()
			return
		}
		n := min(len(e.queue), maxBatch)
		batch := e.queue[:n:n]
		e.queue = e.queue[n:]
		if len(e.queue) == 0 {
			e.queue = nil
		}
		e.cond.Broadcast() // room for AddWait
		e.mu.Unlock()

		e.wmu.Lock()
		var recs []record.Record
		for _, it := range batch {
			if it.fill == nil {
				recs = append(recs, it.r)
				continue
			}
			if len(recs) > 0 {
				e.write(recs)
				recs = nil
			}
			e.applyFill(it.fill)
		}
		if len(recs) > 0 {
			e.write(recs)
		}
		e.wmu.Unlock()

		e.mu.Lock()
		e.committed += uint64(n)
		e.cond.Broadcast()
		e.mu.Unlock()
	}
}

// Close writes what is queued, stops the writer and closes DuckDB. before
// runs in the writer's place after the last batch (nil: nothing), so day
// files can be marked closed cleanly.
func (e *Engine) Close() error {
	if e.days != nil {
		return e.close(e.closeDays)
	}
	return e.close(nil)
}

func (e *Engine) close(before func() error) error {
	e.mu.Lock()
	if e.closing {
		e.mu.Unlock()
		return nil
	}
	e.closing = true
	e.cond.Broadcast()
	e.mu.Unlock()
	if e.wdone != nil {
		<-e.wdone
	}
	var err error
	if before != nil {
		err = before()
	}
	if cerr := e.closeDB(); err == nil {
		err = cerr
	}
	return err
}

func (e *Engine) closeDB() error {
	e.wc.Close()
	err := e.db.Close()
	if cerr := e.connector.Close(); err == nil {
		err = cerr
	}
	return err
}

// dayOf is the day number in an _id.
func dayOf(id int) int { return id >> 32 }
