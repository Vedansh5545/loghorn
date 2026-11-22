package trace

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/jimbeveridge/loghorn/internal/record"
)

func sid(n int) string { return fmt.Sprintf("10000000000000%02d", n) }

func dbSpan(seq int, id, parent, name string, startMs, endMs int) record.Record {
	r := span(seq, id, parent, name, startMs, endMs)
	r.Fields["span.type"] = "db"
	return r
}

func fixtureTrace(t *testing.T) Trace {
	t.Helper()
	tr, ok := Build(fixture(t), t1, Limit)
	if !ok {
		t.Fatal("no trace")
	}
	return tr
}

func TestSelfTimeFixture(t *testing.T) {
	tr := fixtureTrace(t)
	want := map[string]float64{sid(1): 16, sid(11): 4, sid(12): 14, sid(13): 10}
	for _, n := range tr.Nodes {
		w := 0.0
		if v, ok := want[n.ID]; ok {
			w = v
		} else if n.Name == "SELECT orders" {
			w = 8
		} else if n.Kind == KindSpan && n.Name == "SELECT items" {
			w = 16
		}
		if n.Kind == KindSpan && w == 0 {
			continue // other spans are covered by the summary test
		}
		if n.SelfMs != w {
			t.Errorf("%s %s SelfMs = %v, want %v", n.ID, label(n), n.SelfMs, w)
		}
	}
}

func TestSelfTimeSummary(t *testing.T) {
	tr := fixtureTrace(t)
	want := []NameTime{
		{"SELECT orders", 6, 48, 48}, {"SELECT items", 2, 32, 32}, {"orphan task", 1, 20, 20},
		{"GET /orders", 1, 120, 16}, {"POST /price", 1, 24, 14}, {"SELECT prices", 1, 10, 10},
		{"auth check", 1, 8, 8}, {"call pricing", 1, 28, 4},
	}
	if !reflect.DeepEqual(tr.SelfTime, want) {
		t.Errorf("SelfTime = %+v\nwant %+v", tr.SelfTime, want)
	}
}

func TestCriticalPathFixture(t *testing.T) {
	tr := fixtureTrace(t)
	want := map[string]bool{}
	for _, n := range []int{1, 2, 3, 4, 5, 6, 7, 8, 10, 11, 12, 13} {
		want[sid(n)] = true
	}
	for _, n := range tr.Nodes {
		if n.Critical != want[n.ID] {
			t.Errorf("%s %s Critical = %v, want %v", n.ID, label(n), n.Critical, want[n.ID])
		}
	}
}

func TestSerialRunsFixture(t *testing.T) {
	tr := fixtureTrace(t)
	want := []SerialRun{{Parent: sid(1), First: sid(3),
		Spans: []string{sid(3), sid(4), sid(5), sid(6), sid(7), sid(8)}, Count: 6, WallMs: 48}}
	if !reflect.DeepEqual(tr.SerialRuns, want) {
		t.Errorf("SerialRuns = %+v, want %+v", tr.SerialRuns, want)
	}
}

func TestSelfTimeChildOverrun(t *testing.T) {
	tr, _ := Build([]record.Record{span(1, "a", "", "p", 0, 10), span(2, "b", "a", "c", 5, 30)}, t1, Limit)
	if a, b := byID(tr, "a"), byID(tr, "b"); a.SelfMs != 5 || b.SelfMs != 25 {
		t.Errorf("self = %v, %v; want 5, 25", a.SelfMs, b.SelfMs)
	}
}

func TestSerialRunBrokenByOtherSpan(t *testing.T) {
	recs := []record.Record{
		span(1, "p", "", "parent", 0, 10),
		dbSpan(2, "a", "p", "q", 0, 1), dbSpan(3, "b", "p", "q", 1, 2),
		span(4, "c", "p", "work", 2, 3),
		dbSpan(5, "d", "p", "q", 3, 4), dbSpan(6, "e", "p", "q", 4, 5),
	}
	if tr, _ := Build(recs, t1, Limit); len(tr.SerialRuns) != 0 {
		t.Errorf("runs = %+v, want none", tr.SerialRuns)
	}
	recs = []record.Record{
		span(1, "p", "", "parent", 0, 10),
		dbSpan(2, "a", "p", "q", 0, 1), dbSpan(3, "b", "p", "q", 1, 2), dbSpan(4, "c", "p", "q", 2, 3),
	}
	log := record.Record{Seq: 5, Time: ts(1), Source: record.SourceLog,
		Fields: map[string]any{"trace_id": t1, "span_id": "p", "message": "hi"}}
	tr, _ := Build(append(recs, log), t1, Limit)
	if len(tr.SerialRuns) != 1 || tr.SerialRuns[0].Count != 3 || tr.SerialRuns[0].WallMs != 3 {
		t.Errorf("runs = %+v, want one of 3", tr.SerialRuns)
	}
}

func TestCriticalPathSiblingsOverlap(t *testing.T) {
	tr, _ := Build([]record.Record{
		span(1, "p", "", "parent", 0, 10), span(2, "a", "p", "a", 0, 6), span(3, "b", "p", "b", 2, 9),
	}, t1, Limit)
	for id, want := range map[string]bool{"p": true, "a": false, "b": true} {
		if got := byID(tr, id).Critical; got != want {
			t.Errorf("%s Critical = %v, want %v", id, got, want)
		}
	}
}
