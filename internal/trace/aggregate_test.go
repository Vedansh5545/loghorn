package trace

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/store"
)

func aggFixture(t testing.TB) []record.Record {
	t.Helper()
	st := store.New(store.ModeHistorical, time.Time{})
	if err := store.Preload(st, []string{"../../testdata/web/aggregate.jsonl"}, config.Input{}); err != nil {
		t.Fatal(err)
	}
	recs, _ := st.Snapshot()
	return recs
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestAggregateFixture(t *testing.T) {
	op, ok := Aggregate(aggFixture(t), AggOptions{Name: "GET /orders"})
	if !ok {
		t.Fatal("Aggregate = false")
	}
	if op.Traces != 5 || op.Matched != 5 || op.Capped || op.Cap != AggCap || !near(op.OpTotalMs, 600) ||
		op.OutsideSpans != 1 || op.IncompleteTraces != 1 || op.TruncatedTraces != 0 || op.NodesTruncated {
		t.Errorf("header: %+v", op)
	}
	type row struct {
		id, parent, name                string
		depth, children, traces, calls  int
		perTrace, p50, p95, total, self float64
		totalPct, selfPct               float64
		errors, serial                  int
		serialPerTrace, serialPct       float64
		services                        []string
	}
	api, pr := []string{"api-server"}, []string{"pricing"}
	want := []row{
		{"n0", "", "GET /orders", 0, 3, 5, 5, 1, 100, 200, 600, 300, 100, 50, 1, 0, 0, 0, api},
		{"n1", "n0", "auth check", 1, 0, 5, 5, 1, 10, 10, 50, 50, 50.0 / 6, 50.0 / 6, 0, 0, 0, 0, api},
		{"n2", "n0", "SELECT orders", 1, 0, 4, 14, 3.5, 10, 40, 230, 230, 230.0 / 6, 230.0 / 6, 0, 9, 2.25, 15, api},
		{"n3", "n0", "GET /price", 1, 1, 1, 1, 1, 100, 100, 100, 10, 100.0 / 6, 10.0 / 6, 0, 0, 0, 0, api},
		{"n4", "n3", "GET /price", 2, 1, 1, 1, 1, 90, 90, 90, 70, 15, 70.0 / 6, 0, 0, 0, 0, pr},
		{"n5", "n4", "SELECT prices", 3, 0, 1, 1, 1, 20, 20, 20, 20, 20.0 / 6, 20.0 / 6, 0, 0, 0, 0, pr},
	}
	if len(op.Nodes) != len(want) {
		t.Fatalf("%d nodes; want %d: %+v", len(op.Nodes), len(want), op.Nodes)
	}
	for i, w := range want {
		n := op.Nodes[i]
		got := row{n.ID, n.Parent, n.Name, n.Depth, n.Children, n.Traces, n.Calls,
			n.CallsPerTrace, n.P50Ms, n.P95Ms, n.TotalMs, n.SelfMs, n.TotalPct, n.SelfPct,
			n.Errors, n.SerialCalls, n.SerialPerTrace, n.SerialPct, n.Services}
		ints := got.id == w.id && got.parent == w.parent && got.name == w.name && got.depth == w.depth &&
			got.children == w.children && got.traces == w.traces && got.calls == w.calls &&
			got.errors == w.errors && got.serial == w.serial && reflect.DeepEqual(got.services, w.services)
		floats := near(got.perTrace, w.perTrace) && near(got.p50, w.p50) && near(got.p95, w.p95) &&
			near(got.total, w.total) && near(got.self, w.self) && near(got.totalPct, w.totalPct) &&
			near(got.selfPct, w.selfPct) && near(got.serialPerTrace, w.serialPerTrace) && near(got.serialPct, w.serialPct)
		if !ints || !floats {
			t.Errorf("node %d:\n got %+v\nwant %+v", i, got, w)
		}
	}
	if !near(op.Nodes[2].SerialMs, 90) {
		t.Errorf("n2 serialMs %v", op.Nodes[2].SerialMs)
	}
	for i, typ := range []string{"http.server", "span", "db", "http.client", "http.server", "db"} {
		if op.Nodes[i].Type != typ {
			t.Errorf("node %d type %q; want %q", i, op.Nodes[i].Type, typ)
		}
	}
	t4 := "a4000000000000000000000000000000"
	if s := op.Nodes[0].Slowest; s != (Slowest{t4, "a400000000000001", 200}) {
		t.Errorf("n0 slowest %+v", s)
	}
	if s := op.Nodes[2].Slowest; s != (Slowest{t4, "a400000000000003", 40}) {
		t.Errorf("n2 slowest %+v", s)
	}
}

func TestAggregateCap(t *testing.T) {
	op, ok := Aggregate(aggFixture(t), AggOptions{Name: "GET /orders", Cap: 2})
	if !ok || op.Traces != 2 || op.Matched != 5 || !op.Capped || op.Cap != 2 {
		t.Fatalf("ok=%v %+v", ok, op)
	}
	if n := op.Nodes[0]; n.Calls != 2 || !near(n.TotalMs, 300) {
		t.Errorf("n0 calls %d totalMs %v; want 2, 300", n.Calls, n.TotalMs)
	}
}

func TestAggregateRange(t *testing.T) {
	recs := aggFixture(t)
	from := time.Date(2026, 11, 17, 17, 2, 30, 0, time.UTC)
	op, ok := Aggregate(recs, AggOptions{Name: "GET /orders", From: from})
	if !ok || op.Traces != 2 || op.Matched != 2 || op.Capped || !op.From.Equal(from) {
		t.Errorf("from: ok=%v traces=%d matched=%d capped=%v", ok, op.Traces, op.Matched, op.Capped)
	}
	op, ok = Aggregate(recs, AggOptions{Name: "GET /orders", To: time.Date(2026, 11, 17, 17, 0, 30, 0, time.UTC)})
	if !ok || op.Traces != 1 || op.Nodes[0].Slowest.TraceID != "a1000000000000000000000000000000" {
		t.Errorf("to: ok=%v traces=%d", ok, op.Traces)
	}
}

func TestAggregateService(t *testing.T) {
	recs := aggFixture(t)
	if _, ok := Aggregate(recs, AggOptions{Name: "GET /orders", Service: "pricing"}); ok {
		t.Error("pricing matched")
	}
	for _, svc := range []string{"api-server", "*"} {
		op, ok := Aggregate(recs, AggOptions{Name: "GET /orders", Service: svc})
		if !ok || op.Traces != 5 || op.Service != svc {
			t.Errorf("%s: ok=%v traces=%d", svc, ok, op.Traces)
		}
	}
}

func TestAggregateNoMatch(t *testing.T) {
	recs := aggFixture(t)
	for _, name := range []string{"GET /users/x", "auth check"} {
		if _, ok := Aggregate(recs, AggOptions{Name: name}); ok {
			t.Errorf("%q matched", name)
		}
	}
}

func TestAggregateTwoRoots(t *testing.T) {
	recs := []record.Record{
		span(1, "r1", "", "GET /orders", 0, 40),
		span(2, "c1", "r1", "SELECT orders", 5, 15),
		span(3, "r2", "", "GET /orders", 50, 90),
		span(4, "c2", "r2", "SELECT orders", 55, 65),
	}
	op, ok := Aggregate(recs, AggOptions{Name: "GET /orders"})
	if !ok || op.Traces != 1 || op.Nodes[0].Calls != 1 || op.OutsideSpans != 2 || op.IncompleteTraces != 1 {
		t.Fatalf("ok=%v %+v", ok, op)
	}
	if op.Nodes[0].Slowest.SpanID != "r1" || len(op.Nodes) != 2 || op.Nodes[1].Calls != 1 {
		t.Errorf("nodes %+v", op.Nodes)
	}
}

func TestAggregateZeroDuration(t *testing.T) {
	recs := []record.Record{
		span(1, "r1", "", "GET /orders", 0, 0),
		span(2, "c1", "r1", "SELECT orders", 0, 0),
	}
	op, ok := Aggregate(recs, AggOptions{Name: "GET /orders"})
	if !ok || len(op.Nodes) != 2 || op.OpTotalMs != 0 {
		t.Fatalf("ok=%v %+v", ok, op)
	}
	for _, n := range op.Nodes {
		if n.TotalPct != 0 || n.SelfPct != 0 || n.SerialPct != 0 {
			t.Errorf("%s: pcts %v %v %v", n.Name, n.TotalPct, n.SelfPct, n.SerialPct)
		}
	}
	if _, err := json.Marshal(op); err != nil {
		t.Error(err)
	}
}

func TestAggregateJSONShape(t *testing.T) {
	op, _ := Aggregate(aggFixture(t), AggOptions{Name: "GET /orders"})
	b, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"name", "service", "traces", "matched", "capped", "cap", "opTotalMs", "outsideSpans", "incompleteTraces", "truncatedTraces", "nodesTruncated", "nodes"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing %q", k)
		}
	}
	for _, k := range []string{"from", "to"} {
		if _, ok := m[k]; ok {
			t.Errorf("zero %q not omitted", k)
		}
	}
}

func BenchmarkAggregate(b *testing.B) {
	base := time.Date(2026, 11, 17, 17, 0, 0, 0, time.UTC)
	var recs []record.Record
	seq := 0
	add := func(r record.Record) {
		seq++
		r.Seq = seq
		recs = append(recs, r)
	}
	for k := 0; k < 1000; k++ {
		tid := fmt.Sprintf("%032x", k+1)
		start := base.Add(time.Duration(k) * time.Second)
		for j := 0; j < 50; j++ {
			f := map[string]any{"trace_id": tid, "span_id": fmt.Sprintf("%08x%08x", k+1, j+1), "name": "GET /orders",
				"duration_ms": json.Number("100"), "service.name": "api-server"}
			if j > 0 {
				f["name"] = "SELECT orders " + strconv.Itoa(j%7)
				f["parent_span_id"] = fmt.Sprintf("%08x%08x", k+1, 1+(j-1)/7)
				f["duration_ms"] = json.Number("5")
			}
			add(record.Record{Time: start.Add(time.Duration(j) * time.Millisecond), Source: record.SourceOtel, Fields: f})
		}
		for j := 0; j < 200; j++ {
			add(record.Record{Time: start, Source: record.SourceLog, Fields: map[string]any{"message": "hello"}})
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if op, ok := Aggregate(recs, AggOptions{Name: "GET /orders"}); !ok || op.Traces != 1000 {
			b.Fatalf("ok=%v traces=%d", ok, op.Traces)
		}
	}
}
