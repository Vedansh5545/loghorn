package trace

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/store"
)

const (
	t1 = "4bf92f3577b34da6a3ce929d0e0e4736"
	t2 = "5ce0e4736a3ce929d4bf92f3577b34da"
)

func fixture(t *testing.T) []record.Record {
	t.Helper()
	st := store.New(store.ModeHistorical, time.Time{})
	if err := store.Preload(st, []string{"../../testdata/web/trace.jsonl"}, config.Input{}); err != nil {
		t.Fatal(err)
	}
	recs, _ := st.Snapshot()
	return recs
}

func label(n Node) string {
	switch n.Kind {
	case KindLog:
		return n.Message
	case KindMissing:
		return "(missing " + n.Name + ")"
	}
	return n.Name
}

func byID(tr Trace, id string) Node {
	for _, n := range tr.Nodes {
		if n.ID == id {
			return n
		}
	}
	return Node{}
}

func TestBuildFixture(t *testing.T) {
	tr, ok := Build(fixture(t), t1, Limit)
	if !ok {
		t.Fatal("Build = false")
	}
	want := []struct {
		label string
		depth int
	}{
		{"GET /orders", 0}, {"request received", 1}, {"auth check", 1}, {"token ok", 2},
		{"SELECT orders", 1}, {"SELECT orders", 1}, {"SELECT orders", 1},
		{"SELECT orders", 1}, {"SELECT orders", 1}, {"SELECT orders", 1},
		{"SELECT items", 1}, {"SELECT items", 1}, {"call pricing", 1}, {"calling pricing", 2},
		{"POST /price", 2}, {"SELECT prices", 3}, {"query timed out", 4}, {"response sent", 1},
		{"(missing ffffffffffffff01)", 0}, {"orphan task", 1},
		{"(missing ffffffffffffff02)", 0}, {"late callback", 1},
	}
	if len(tr.Nodes) != len(want) {
		t.Fatalf("%d nodes; want %d", len(tr.Nodes), len(want))
	}
	for i, w := range want {
		if got := label(tr.Nodes[i]); got != w.label || tr.Nodes[i].Depth != w.depth {
			t.Errorf("node %d = %q depth %d; want %q depth %d", i, got, tr.Nodes[i].Depth, w.label, w.depth)
		}
	}
	if tr.SpanCount != 14 || tr.LogCount != 6 || tr.DurationMs != 120 || tr.Truncated {
		t.Errorf("spans %d logs %d duration %v truncated %v", tr.SpanCount, tr.LogCount, tr.DurationMs, tr.Truncated)
	}
	if !reflect.DeepEqual(tr.Services, []string{"api-server", "pricing"}) {
		t.Errorf("services = %v", tr.Services)
	}
	if want := time.Date(2026, 11, 17, 17, 0, 0, 0, time.UTC); !tr.Start.Equal(want) {
		t.Errorf("start = %v", tr.Start)
	}
	if p := byID(tr, "1000000000000014").Parent; p != "missing:ffffffffffffff01" {
		t.Errorf("orphan parent = %q", p)
	}
	if !byID(tr, "1000000000000013").Error {
		t.Error("s13 is not an error")
	}
	ticks := map[string][]float64{
		"1000000000000001": {1, 115}, "1000000000000002": {5},
		"1000000000000011": {83}, "1000000000000013": {95}, "missing:ffffffffffffff02": {40},
	}
	for id, w := range ticks {
		if got := byID(tr, id).LogTicks; !reflect.DeepEqual(got, w) {
			t.Errorf("%s LogTicks = %v; want %v", id, got, w)
		}
	}
	if ph := byID(tr, "missing:ffffffffffffff01"); ph.StartMs != 30 || ph.DurationMs != 20 {
		t.Errorf("placeholder = %v + %v", ph.StartMs, ph.DurationMs)
	}
	if c := byID(tr, "1000000000000001").Children; c != 12 {
		t.Errorf("s01 children = %d", c)
	}
	if tr.SerialRuns == nil || tr.SelfTime == nil {
		t.Error("SerialRuns/SelfTime must be empty slices")
	}
	if _, err := json.Marshal(tr); err != nil {
		t.Error(err)
	}
}

func TestBuildOtherTrace(t *testing.T) {
	tr, ok := Build(fixture(t), t2, Limit)
	if !ok || len(tr.Nodes) != 2 {
		t.Fatalf("ok=%v nodes=%d", ok, len(tr.Nodes))
	}
	if label(tr.Nodes[0]) != "GET /health" || label(tr.Nodes[1]) != "health ok" || tr.Nodes[1].Depth != 1 {
		t.Errorf("nodes = %+v", tr.Nodes)
	}
	if tr.DurationMs != 1 {
		t.Errorf("duration = %v", tr.DurationMs)
	}
}

func TestBuildUnknown(t *testing.T) {
	if _, ok := Build(fixture(t), "deadbeef", Limit); ok {
		t.Error("Build found a trace that is not there")
	}
}

func TestFind(t *testing.T) {
	recs := fixture(t)
	for _, id := range []string{"1000000000000013", "ffffffffffffff02", "FFFFFFFFFFFFFF02", "1000000000000011"} {
		if got := Find(recs, id); got != t1 {
			t.Errorf("Find(%q) = %q", id, got)
		}
	}
	if got := Find(recs, "2000000000000001"); got != t2 {
		t.Errorf("Find(T2 root) = %q", got)
	}
	if got := Find(recs, "abcdef0123456789"); got != "" {
		t.Errorf("Find(unknown) = %q", got)
	}
}

func TestBuildTruncated(t *testing.T) {
	tr, ok := Build(fixture(t), t1, 5)
	if !ok || tr.SpanCount != 5 || tr.LogCount != 5 || !tr.Truncated {
		t.Errorf("ok=%v spans=%d logs=%d truncated=%v", ok, tr.SpanCount, tr.LogCount, tr.Truncated)
	}
}

func ts(ms int) time.Time {
	return time.Date(2026, 11, 17, 17, 0, 0, 0, time.UTC).Add(time.Duration(ms) * time.Millisecond)
}

func span(seq int, id, parent, name string, startMs, endMs int) record.Record {
	f := map[string]any{
		"trace_id": t1, "span_id": id, "name": name,
		"end":         ts(endMs).Format(time.RFC3339Nano),
		"duration_ms": json.Number(strconv.Itoa(endMs - startMs)),
	}
	if parent != "" {
		f["parent_span_id"] = parent
	}
	return record.Record{Seq: seq, Time: ts(startMs), Source: record.SourceOtel, Fields: f}
}

func TestBuildCycle(t *testing.T) {
	recs := []record.Record{
		span(1, "a", "b", "a", 0, 10), span(2, "b", "a", "b", 1, 9), span(3, "c", "c", "c", 2, 8),
	}
	tr, ok := Build(recs, t1, Limit)
	if !ok || len(tr.Nodes) != 3 {
		t.Fatalf("ok=%v nodes=%d", ok, len(tr.Nodes))
	}
	seen := map[string]bool{}
	for _, n := range tr.Nodes {
		if seen[n.ID] || n.Depth > 1 {
			t.Errorf("node %s repeated or too deep (%d)", n.ID, n.Depth)
		}
		seen[n.ID] = true
	}
}

func TestBuildDuplicateSpanID(t *testing.T) {
	recs := []record.Record{span(2, "a", "", "second", 5, 6), span(1, "a", "", "first", 0, 1)}
	tr, _ := Build(recs, t1, Limit)
	if len(tr.Nodes) != 1 || tr.Nodes[0].Name != "first" || tr.SpanCount != 1 {
		t.Errorf("nodes = %+v spans %d", tr.Nodes, tr.SpanCount)
	}
}

func TestBuildTimes(t *testing.T) {
	noEnd := span(1, "a", "", "a", 0, 0)
	delete(noEnd.Fields, "end")
	noEnd.Fields["duration_ms"] = json.Number("7")
	backwards := span(2, "b", "", "b", 20, 10)
	tr, _ := Build([]record.Record{noEnd, backwards}, t1, Limit)
	if d := byID(tr, "a").DurationMs; d != 7 {
		t.Errorf("no end: duration = %v", d)
	}
	if d := byID(tr, "b").DurationMs; d != 0 {
		t.Errorf("end before start: duration = %v", d)
	}

	log := record.Record{Seq: 1, Time: ts(3), Fields: map[string]any{"trace_id": t1, "message": "only"}}
	tr, ok := Build([]record.Record{log}, t1, Limit)
	if !ok || tr.DurationMs != 0 {
		t.Fatalf("ok=%v duration=%v", ok, tr.DurationMs)
	}
	if _, err := json.Marshal(tr); err != nil {
		t.Error(err)
	}
}

func TestBuildLongMessage(t *testing.T) {
	log := record.Record{Seq: 1, Time: ts(0), Fields: map[string]any{"trace_id": t1, "message": strings.Repeat("é", 600)}}
	tr, _ := Build([]record.Record{log}, t1, Limit)
	if n := len([]rune(tr.Nodes[0].Message)); n != maxMessage {
		t.Errorf("message has %d runes", n)
	}
}
