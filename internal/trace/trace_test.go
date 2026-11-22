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
	case KindEvent:
		return "◆ " + n.Name
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
		{"GET /orders", 0}, {"request received", 1}, {"auth check", 1}, {"token ok", 2}, {"◆ cache miss", 1},
		{"SELECT orders", 1}, {"SELECT orders", 1}, {"SELECT orders", 1},
		{"SELECT orders", 1}, {"SELECT orders", 1}, {"SELECT orders", 1},
		{"SELECT items", 1}, {"SELECT items", 1}, {"call pricing", 1}, {"calling pricing", 2},
		{"POST /price", 2}, {"SELECT prices", 3}, {"query timed out", 4}, {"◆ exception", 4}, {"response sent", 1},
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
	if tr.EventCount != 2 {
		t.Errorf("events %d", tr.EventCount)
	}
	if got := byID(tr, "1000000000000013").EventTicks; !reflect.DeepEqual(got, []EventTick{{Ms: 99, Error: true}}) {
		t.Errorf("s13 EventTicks = %v", got)
	}
	if got := byID(tr, "event:1000000000000013:0").Message; got != "TimeoutError: statement timeout after 10ms" {
		t.Errorf("exception message = %q", got)
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
	if c := byID(tr, "1000000000000001").Children; c != 13 {
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

// Placeholders with equal starts come out in ID order on every build.
func TestBuildPlaceholderOrderStable(t *testing.T) {
	recs := []record.Record{
		span(1, "a1", "pz", "a", 0, 5), span(2, "b1", "py", "b", 0, 5),
		span(3, "c1", "px", "c", 0, 5), span(4, "d1", "pw", "d", 0, 5),
	}
	for i := 0; i < 30; i++ {
		tr, _ := Build(recs, t1, Limit)
		var got []string
		for _, n := range tr.Nodes {
			if n.Kind == KindMissing {
				got = append(got, n.ID)
			}
		}
		want := []string{"missing:pw", "missing:px", "missing:py", "missing:pz"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: placeholders = %v; want %v", i, got, want)
		}
	}
}

// Equal instants in different locations still fall through to the Seq
// tie-breaker: the placeholder (Seq 0) sorts before the root span.
func TestBuildEqualStartsDifferentZones(t *testing.T) {
	root := span(1, "r", "", "root", 0, 5)
	orphan := span(2, "o", "gone", "orphan", 0, 5)
	orphan.Time = orphan.Time.In(time.FixedZone("x", 3600))
	tr, _ := Build([]record.Record{root, orphan}, t1, Limit)
	if len(tr.Nodes) < 1 || tr.Nodes[0].Kind != KindMissing {
		t.Errorf("first node = %+v; want the placeholder", tr.Nodes)
	}
}

func event(ms int, name string, attrs map[string]any) any {
	ev := map[string]any{"name": name}
	if ms >= 0 {
		ev["time"] = ts(ms).Format(time.RFC3339Nano)
	}
	if attrs != nil {
		ev["attributes"] = attrs
	}
	return ev
}

// Span events are children of their span, in time order with its logs; one
// after the span ends still gets its real time and stretches the trace.
func TestBuildEvents(t *testing.T) {
	root := span(1, "a", "", "root", 0, 10)
	root.Fields["events"] = []any{
		event(4, "cache miss", map[string]any{"hit": false, "n": json.Number("2")}),
		event(15, "late", nil),
		event(6, "exception", map[string]any{"exception.type": "IOError", "exception.message": "disk full"}),
		event(-1, "untimed", nil),
	}
	log := record.Record{Seq: 2, Time: ts(5), Fields: map[string]any{"trace_id": t1, "span_id": "a", "message": "in a"}}
	tr, ok := Build([]record.Record{root, log}, t1, Limit)
	if !ok {
		t.Fatal("Build = false")
	}
	var got []string
	for _, n := range tr.Nodes {
		got = append(got, n.ID)
	}
	want := []string{"a", "event:a:3", "event:a:0", "log:2", "event:a:2", "event:a:1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nodes = %v; want %v", got, want)
	}
	ev := byID(tr, "event:a:0")
	if ev.Kind != KindEvent || ev.Parent != "a" || ev.Depth != 1 || ev.Name != "cache miss" || ev.StartMs != 4 ||
		ev.Error || ev.Seq != 0 || ev.Time != ts(4).Format(time.RFC3339Nano) {
		t.Errorf("cache miss = %+v", ev)
	}
	if ev.Attributes["hit"] != false || ev.Attributes["n"] != json.Number("2") {
		t.Errorf("attributes = %v", ev.Attributes)
	}
	if ex := byID(tr, "event:a:2"); !ex.Error || ex.Message != "IOError: disk full" {
		t.Errorf("exception = %+v", ex)
	}
	if late := byID(tr, "event:a:1"); late.StartMs != 15 || late.DurationMs != 0 {
		t.Errorf("late = %+v", late)
	}
	if u := byID(tr, "event:a:3"); u.StartMs != 0 || u.Time != "" {
		t.Errorf("untimed = %+v", u)
	}
	a := byID(tr, "a")
	if a.Children != 5 || !reflect.DeepEqual(a.LogTicks, []float64{5}) {
		t.Errorf("a children %d logTicks %v", a.Children, a.LogTicks)
	}
	wantTicks := []EventTick{{Ms: 0}, {Ms: 4}, {Ms: 6, Error: true}, {Ms: 15}}
	if !reflect.DeepEqual(a.EventTicks, wantTicks) {
		t.Errorf("eventTicks = %+v; want %+v", a.EventTicks, wantTicks)
	}
	if a.SelfMs != 10 || !a.Critical || a.Error {
		t.Errorf("a self %v critical %v error %v", a.SelfMs, a.Critical, a.Error)
	}
	if tr.DurationMs != 15 || tr.EventCount != 4 || tr.SpanCount != 1 || tr.LogCount != 1 {
		t.Errorf("duration %v events %d spans %d logs %d", tr.DurationMs, tr.EventCount, tr.SpanCount, tr.LogCount)
	}
	b, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"kind":"event"`) || !strings.Contains(string(b), `"eventTicks":[`) {
		t.Errorf("json = %s", b)
	}
}

// An event before its span starts moves the trace start; an entry that is not
// an object is skipped, and one with a bad time sits at its span's start.
func TestBuildEventEarlyAndMalformed(t *testing.T) {
	root := span(1, "a", "", "root", 10, 20)
	root.Fields["events"] = []any{"junk", map[string]any{"time": "nope"}, event(2, "early", nil)}
	tr, _ := Build([]record.Record{root}, t1, Limit)
	if len(tr.Nodes) != 3 || tr.EventCount != 2 {
		t.Fatalf("nodes = %+v", tr.Nodes)
	}
	if !tr.Start.Equal(ts(2)) || tr.Nodes[0].StartMs != 8 {
		t.Errorf("start %v root %+v", tr.Start, tr.Nodes[0])
	}
	if e := tr.Nodes[1]; e.ID != "event:a:2" || e.StartMs != 0 {
		t.Errorf("early = %+v", e)
	}
	if e := tr.Nodes[2]; e.ID != "event:a:1" || e.StartMs != 8 || e.Name != "" {
		t.Errorf("bad time = %+v", e)
	}
}

func TestBuildEventsTruncated(t *testing.T) {
	root := span(1, "a", "", "root", 0, 10)
	root.Fields["events"] = []any{event(1, "e1", nil), event(2, "e2", nil), event(3, "e3", nil)}
	tr, _ := Build([]record.Record{root}, t1, 2)
	if tr.EventCount != 2 || !tr.Truncated {
		t.Errorf("events %d truncated %v", tr.EventCount, tr.Truncated)
	}
}

// An exception event's label falls back to whichever of type and message it has.
func TestEventExceptionMessage(t *testing.T) {
	for _, c := range []struct {
		attrs map[string]any
		want  string
	}{
		{map[string]any{"exception.type": "E"}, "E"},
		{map[string]any{"exception.message": "m"}, "m"},
		{nil, ""},
	} {
		root := span(1, "a", "", "root", 0, 10)
		root.Fields["events"] = []any{event(1, "exception", c.attrs)}
		tr, _ := Build([]record.Record{root}, t1, Limit)
		if n := byID(tr, "event:a:0"); n.Message != c.want || !n.Error {
			t.Errorf("%v: %+v", c.attrs, n)
		}
	}
}

// Events neither take self time from their span nor cut the critical path.
func TestEventsDoNotChangeAnalysis(t *testing.T) {
	root := span(1, "a", "", "root", 0, 10)
	root.Fields["events"] = []any{event(9, "done", nil)}
	child := span(2, "b", "a", "child", 2, 8)
	tr, _ := Build([]record.Record{root, child}, t1, Limit)
	if a := byID(tr, "a"); a.SelfMs != 4 || !a.Critical {
		t.Errorf("a = %+v", a)
	}
	if !byID(tr, "b").Critical {
		t.Error("child is not on the critical path")
	}
}
