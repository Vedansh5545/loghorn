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

// StreamCount is a stream's name, source and record count. The same name
// under two sources is two streams.
type StreamCount struct {
	Name   string
	Source string
	Count  int
}

// Store holds the session's records.
type Store struct {
	mu       sync.RWMutex
	mode     Mode
	runStart time.Time
	recs     []record.Record
	streams  map[record.StreamKey]int
	pending  []int // indexes of leading records with no time yet
	earliest time.Time
	latest   time.Time
	fallback string // service of records that name none
}

// New returns an empty store. runStart is zero for replay modes.
func New(mode Mode, runStart time.Time) *Store {
	return &Store{mode: mode, runStart: runStart, streams: map[record.StreamKey]int{}, fallback: record.DefaultService}
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

// Add appends r, assigning Seq and its effective time. A zero r.Time takes
// received when non-zero, else the previous record's Time; with neither it
// stays zero and is backfilled by the next record with a real time.
func (s *Store) Add(r record.Record, received time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.Seq = len(s.recs) + 1
	if r.Service == "" {
		r.Service = s.fallback
	}
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
	s.streams[r.Key()]++
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

// Streams returns per-stream counts sorted by name, then source.
func (s *Store) Streams() []StreamCount {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]StreamCount, 0, len(s.streams))
	for k, c := range s.streams {
		out = append(out, StreamCount{Name: k.Name, Source: k.Source, Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Source < out[j].Source
	})
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

// Count is a name and how many records carry it.
type Count struct {
	Name  string
	Count int
}

// Kinds counts, in one pass under the read lock, the records of source ("" is
// log) whose time is in [from, to). A zero bound is unbounded; a record with
// no time counts only when from is zero. services counts per service,
// ignoring service; kinds counts per Stream, limited to service unless it is
// "*". Both sort by count descending, then name, and are never nil.
func (s *Store) Kinds(source, service string, from, to time.Time) (kinds, services []Count) {
	src, ok := record.ParseSource(source)
	kc := map[string]int{}
	sc := map[string]int{}
	if ok {
		s.mu.RLock()
		for i := range s.recs {
			r := &s.recs[i]
			rs := r.Source
			if rs == "" {
				rs = record.SourceLog
			}
			if rs != src {
				continue
			}
			if r.Time.IsZero() {
				if !from.IsZero() {
					continue
				}
			} else if (!from.IsZero() && r.Time.Before(from)) || (!to.IsZero() && !r.Time.Before(to)) {
				continue
			}
			sc[r.Service]++
			if service == "*" || r.Service == service {
				kc[r.Stream]++
			}
		}
		s.mu.RUnlock()
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
