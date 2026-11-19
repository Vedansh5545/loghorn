// Package recorder is the single write point for input records: it copies each
// to the log file, parses it, stores it in the live store and raises alerts.
package recorder

import (
	"bytes"
	"encoding/json"
	"sync"
	"time"

	"github.com/jimbeveridge/loghorn/internal/adapter"
	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/engine"
	"github.com/jimbeveridge/loghorn/internal/entry"
	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/store"
)

// Options configures a Recorder.
type Options struct {
	Sink            func(rec []byte) // copies a record to the log file; nil = none
	Store           *store.Store     // live web store; nil = none
	Input           config.Input
	Live            bool                // stamp store records with arrival time (false for replays)
	Alert           func(e entry.Entry) // called with each stored important entry; nil = none
	DedupeAfterOTLP bool                // once an OTLP record arrives, stop storing input lines that are JSON objects
	Now             func() time.Time    // nil = time.Now
}

// Recorder serialises records from concurrent sources.
type Recorder struct {
	o      Options
	mu     sync.Mutex
	parser *adapter.Parser
	seen   bool
}

// New returns a Recorder.
func New(o Options) *Recorder {
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Recorder{o: o, parser: adapter.New(o.Input)}
}

// Line records one framed input record and returns its parsed entry, whether
// or not it was stored. rec is only valid during the call.
func (r *Recorder) Line(rec []byte) entry.Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.o.DedupeAfterOTLP && r.seen && isJSONObject(rec) {
		e := r.parser.ParseLine(rec)
		e.Received = r.o.Now()
		e.Important = engine.IsImportant(e)
		return e
	}
	return r.store(rec)
}

// OTLP records one mapped OTLP line and marks OTLP as seen.
func (r *Recorder) OTLP(rec []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = true
	r.store(rec)
}

// SawOTLP reports whether an OTLP record has arrived.
func (r *Recorder) SawOTLP() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen
}

// store runs the full path; the caller holds mu.
func (r *Recorder) store(rec []byte) entry.Entry {
	if r.o.Sink != nil {
		r.o.Sink(rec)
	}
	e := r.parser.ParseLine(rec)
	now := r.o.Now()
	e.Received = now
	e.Important = engine.IsImportant(e)
	if r.o.Store != nil {
		received := time.Time{}
		if r.o.Live {
			received = now
		}
		if rc, ok := record.Normalize(e, r.o.Input); ok {
			r.o.Store.Add(rc, received)
		}
	}
	if e.Important && r.o.Alert != nil {
		r.o.Alert(e)
	}
	return e
}

func isJSONObject(rec []byte) bool {
	t := bytes.TrimSpace(rec)
	return len(t) > 0 && t[0] == '{' && json.Valid(t)
}
