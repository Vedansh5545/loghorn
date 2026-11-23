package query

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/schema"
)

func TestRound15(t *testing.T) {
	cases := []struct{ in, want float64 }{
		{23.839999999999996, 23.84},
		{0.1 + 0.2, 0.3},
		{1e-20, 1e-20},
		{123456789012345678, 123456789012346000},
		{0, 0},
		{-0.30000000000000004, -0.3},
	}
	for _, c := range cases {
		if got := Round15(c.in); got != c.want {
			t.Errorf("Round15(%v) = %v; want %v", c.in, got, c.want)
		}
	}
	if !math.IsNaN(Round15(math.NaN())) {
		t.Error("NaN changed")
	}
	if !math.IsInf(Round15(math.Inf(1)), 1) || !math.IsInf(Round15(math.Inf(-1)), -1) {
		t.Error("Inf changed")
	}
}

func TestAggregateAvgRounded(t *testing.T) {
	s := sch(latF)
	recs := []record.Record{rec(1, t0, "x", "lat", json.Number("0.1")), rec(2, t0, "x", "lat", json.Number("0.2"))}
	res := run(t, recs, s, Query{View: "table", Aggregates: []Aggregate{{Op: "avg", Field: "lat"}, {Op: "sum", Field: "lat"}, {Op: "max", Field: "lat"}}})
	b, err := json.Marshal(res.Rows[0].Cells[1:])
	if err != nil {
		t.Fatal(err)
	}
	if want := `[{"v":0.15},{"v":0.3},{"v":0.2}]`; string(b) != want {
		t.Fatalf("cells = %s; want %s", b, want)
	}
}

func TestSumIntNotRounded(t *testing.T) {
	n := schema.Field{Path: "n", Type: schema.TInt, Visualizer: "number"}
	recs := []record.Record{rec(1, t0, "x", "n", json.Number("1234567890123456")), rec(2, t0, "x", "n", json.Number("1"))}
	res := run(t, recs, sch(n), Query{View: "table", Aggregates: []Aggregate{{Op: "sum", Field: "n"}}})
	if got := res.Rows[0].Cells[1].V; got != int64(1234567890123457) {
		t.Fatalf("sum = %v (%T)", got, got)
	}
}
