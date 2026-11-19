package recorder

import (
	"sync"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/entry"
	"github.com/jimbeveridge/loghorn/internal/store"
)

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

const (
	lineA = `{"labels":{"schema":"web"},"severity":"INFO","message":"a"}`
	lineB = `plain b`
	lineC = `{"labels":{"schema":"web"},"severity":"INFO","message":"c"}`
	lineD = `plain d`
	lineE = `{not json`
	otlpL = `{"labels":{"schema":"otel"},"severity":"INFO","message":"o"}`
)

type rig struct {
	sink   []string
	alerts []entry.Entry
	st     *store.Store
	r      *Recorder
}

func newRig(o Options) *rig {
	g := &rig{st: store.New(store.ModeLive, t0)}
	o.Sink = func(rec []byte) { g.sink = append(g.sink, string(rec)) }
	o.Store = g.st
	o.Alert = func(e entry.Entry) { g.alerts = append(g.alerts, e) }
	g.r = New(o)
	return g
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

func TestLineStoresEverywhere(t *testing.T) {
	g := newRig(Options{Live: true})
	e := g.r.Line([]byte(`{"labels":{"schema":"web"},"severity":"ERROR","message":"boom"}`))
	if e.Severity != entry.SevError || !e.Important {
		t.Errorf("entry = %+v", e)
	}
	e2 := g.r.Line([]byte(lineB))
	if e2.Important {
		t.Error("plain line marked important")
	}
	if len(g.sink) != 2 || g.sink[1] != lineB {
		t.Errorf("sink = %q", g.sink)
	}
	recs, _ := g.st.Snapshot()
	if len(recs) != 1 || recs[0].Stream != "web" {
		t.Errorf("store = %+v", recs)
	}
}

func TestReplayHasNoReceivedTime(t *testing.T) {
	now := t0.Add(time.Hour)
	g := newRig(Options{Live: false, Now: func() time.Time { return now }})
	g.r.Line([]byte(`{"labels":{"schema":"web"},"message":"first"}`))
	g.r.Line([]byte(`{"labels":{"schema":"web"},"message":"second","timestamp":"2026-03-04T05:06:07Z"}`))
	recs, _ := g.st.Snapshot()
	if len(recs) != 2 {
		t.Fatalf("recs = %d", len(recs))
	}
	want := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if !recs[0].Time.Equal(want) {
		t.Errorf("first Time = %v, want %v", recs[0].Time, want)
	}
}

func feed(g *rig) (c entry.Entry) {
	g.r.Line([]byte(lineA))
	g.r.Line([]byte(lineB))
	g.r.OTLP([]byte(otlpL))
	c = g.r.Line([]byte(lineC))
	g.r.Line([]byte(lineD))
	g.r.Line([]byte(lineE))
	return c
}

func TestDedupeAfterOTLP(t *testing.T) {
	g := newRig(Options{DedupeAfterOTLP: true})
	c := feed(g)
	eq(t, g.sink, []string{lineA, lineB, otlpL, lineD, lineE})
	if !g.r.SawOTLP() {
		t.Error("SawOTLP false")
	}
	if c.Message != "c" {
		t.Errorf("deduped line not parsed: %+v", c)
	}
	recs, _ := g.st.Snapshot()
	if len(recs) != 2 {
		t.Errorf("store has %d, want 2 (A and otlp)", len(recs))
	}
}

func TestNoDedupeWithoutOption(t *testing.T) {
	g := newRig(Options{})
	feed(g)
	eq(t, g.sink, []string{lineA, lineB, otlpL, lineC, lineD, lineE})
}

func TestAlertOnlyForStoredImportant(t *testing.T) {
	g := newRig(Options{DedupeAfterOTLP: true})
	g.r.Line([]byte(`{"labels":{"schema":"web"},"severity":"ERROR","message":"pre"}`))
	g.r.Line([]byte(lineA))
	g.r.OTLP([]byte(`{"labels":{"schema":"otel"},"severity":"ERROR","message":"otlp"}`))
	g.r.Line([]byte(`{"labels":{"schema":"web"},"severity":"ERROR","message":"post"}`))
	var got []string
	for _, e := range g.alerts {
		got = append(got, e.Message)
	}
	eq(t, got, []string{"pre", "otlp"})
}

func TestConcurrentLineAndOTLP(t *testing.T) {
	var mu sync.Mutex
	n := 0
	st := store.New(store.ModeLive, t0)
	r := New(Options{Store: st, Sink: func([]byte) { mu.Lock(); n++; mu.Unlock() }})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				r.Line([]byte(lineA))
			}
		}()
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				r.OTLP([]byte(otlpL))
			}
		}()
	}
	wg.Wait()
	if n != 3000 {
		t.Errorf("sink = %d", n)
	}
	if recs, _ := st.Snapshot(); len(recs) != 3000 {
		t.Errorf("store = %d", len(recs))
	}
}
