package serve

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/otlp"
	"github.com/jimbeveridge/loghorn/internal/recorder"
	"github.com/jimbeveridge/loghorn/internal/store"
)

// idleRun is how long a run may send nothing before its recorder is dropped.
// A run that resumes later gets a fresh one with the same settings.
const idleRun = 10 * time.Minute

// router sends each arriving record through the right recorder: a run's lines
// through that run's own (its input settings, its service), OTLP logs through
// the general one. A run whose command also sends OTLP gets the dedupe today's
// wrapper had: once OTLP with its run id arrives, its JSON stdout lines are no
// longer stored.
type router struct {
	sink    func([]byte)
	st      *store.Store
	input   config.Input
	now     func() time.Time
	general *recorder.Recorder

	mu     sync.Mutex
	runs   map[string]*runRec
	marked map[string]time.Time // OTLP seen for a run that hasn't sent a line yet
}

type runRec struct {
	rec  *recorder.Recorder
	last time.Time
}

func newRouter(sink func([]byte), st *store.Store, in config.Input, now func() time.Time) *router {
	return &router{
		sink: sink, st: st, input: in, now: now,
		general: recorder.New(recorder.Options{Sink: sink, Store: st, Input: in, Live: true, Now: now}),
		runs:    map[string]*runRec{},
		marked:  map[string]time.Time{},
	}
}

func (r *router) runLine(l otlp.RunLine) {
	r.recorderFor(l).Line(l.Line)
}

func (r *router) recorderFor(l otlp.RunLine) *recorder.Recorder {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if rr, ok := r.runs[l.RunID]; ok {
		rr.last = now
		return rr.rec
	}
	in := r.input
	if len(l.Input) > 0 {
		// Decode over serve's own settings, so a key the run left out keeps
		// serve's value; a broken document leaves them all.
		try := r.input
		if json.Unmarshal(l.Input, &try) == nil {
			in = try
		}
	}
	rec := recorder.New(recorder.Options{
		Sink: taggedSink(r.sink, l.Service, l.RunID), Store: r.st, Input: in, Live: true, Now: r.now,
		DedupeAfterOTLP: true, Service: l.Service,
	})
	if _, ok := r.marked[l.RunID]; ok {
		rec.MarkOTLP()
		delete(r.marked, l.RunID)
	}
	r.runs[l.RunID] = &runRec{rec: rec, last: now}
	return rec
}

func (r *router) otlpLog(line []byte) {
	r.general.OTLP(line)
	var x struct {
		RunID string `json:"loghorn.run.id"`
	}
	if json.Unmarshal(line, &x) != nil || x.RunID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if rr, ok := r.runs[x.RunID]; ok {
		rr.rec.MarkOTLP()
		return
	}
	r.marked[x.RunID] = r.now()
}

// evict forgets runs and marks last touched before cutoff.
func (r *router) evict(cutoff time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, rr := range r.runs {
		if rr.last.Before(cutoff) {
			delete(r.runs, id)
		}
	}
	for id, t := range r.marked {
		if t.Before(cutoff) {
			delete(r.marked, id)
		}
	}
}
