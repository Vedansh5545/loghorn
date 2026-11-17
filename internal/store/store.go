// Package store is the in-memory session store behind the web query view.
// Records are immutable once they leave the lock; Snapshot returns a copy.
package store

import (
	"bytes"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/jimbeveridge/loghorn/internal/adapter"
	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/ingest"
	"github.com/jimbeveridge/loghorn/internal/record"
)

// Mode says where the session's records come from.
type Mode string

const (
	ModeLive       Mode = "live"
	ModeHistorical Mode = "historical"
	ModeGHJob      Mode = "gh-job"
)

// Info summarises the session.
type Info struct {
	Mode             Mode
	RunStart         time.Time
	Earliest, Latest time.Time
	Count            int
}

// StreamCount is a stream name and its record count.
type StreamCount struct {
	Name  string
	Count int
}

// Store holds the session's records.
type Store struct {
	mu       sync.RWMutex
	mode     Mode
	runStart time.Time
	recs     []record.Record
	streams  map[string]int
	pending  []int // indexes of leading records with no time yet
	earliest time.Time
	latest   time.Time
}

// New returns an empty store. runStart is zero for replay modes.
func New(mode Mode, runStart time.Time) *Store {
	return &Store{mode: mode, runStart: runStart, streams: map[string]int{}}
}

// Add appends r, assigning Seq and its effective time. A zero r.Time takes
// received when non-zero, else the previous record's Time; with neither it
// stays zero and is backfilled by the next record with a real time.
func (s *Store) Add(r record.Record, received time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.Seq = len(s.recs) + 1
	if r.Time.IsZero() {
		r.TimeInherited = true
		switch {
		case !received.IsZero():
			r.Time = received
		case len(s.recs) > 0:
			r.Time = s.recs[len(s.recs)-1].Time
		}
	}
	if r.Time.IsZero() {
		s.pending = append(s.pending, len(s.recs))
	} else {
		for _, p := range s.pending {
			s.recs[p].Time = r.Time
		}
		s.pending = nil
		s.widen(r.Time)
	}
	s.recs = append(s.recs, r)
	s.streams[r.Stream]++
}

func (s *Store) widen(t time.Time) {
	if s.earliest.IsZero() || t.Before(s.earliest) {
		s.earliest = t
	}
	if t.After(s.latest) {
		s.latest = t
	}
}

// Snapshot returns a copy of the record slice and its version (the count).
func (s *Store) Snapshot() ([]record.Record, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]record.Record, len(s.recs))
	copy(out, s.recs)
	return out, uint64(len(out))
}

// Streams returns per-stream counts sorted by name.
func (s *Store) Streams() []StreamCount {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]StreamCount, 0, len(s.streams))
	for n, c := range s.streams {
		out = append(out, StreamCount{n, c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Info returns the session summary. Earliest/Latest ignore zero times.
func (s *Store) Info() Info {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Info{Mode: s.mode, RunStart: s.runStart, Earliest: s.earliest, Latest: s.latest, Count: len(s.recs)}
}

// Preload reads the stored log files into s, in order, with no received time.
func Preload(s *Store, paths []string, in config.Input) error {
	p := adapter.New(in)
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
