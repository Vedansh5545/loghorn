package query

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/jimbeveridge/loghorn/internal/record"
)

// orderRecs: a has 3 records (lat 1,2,3 → avg 2), b has 1 (lat 10),
// c has 2 with no lat (no avg), d has 2 (lat 4,6 → avg 5).
func orderRecs() []record.Record {
	n := func(s string) json.Number { return json.Number(s) }
	return []record.Record{
		rec(1, t0, "x", "route", "a", "lat", n("1")),
		rec(2, t0, "x", "route", "a", "lat", n("2")),
		rec(3, t0, "x", "route", "a", "lat", n("3")),
		rec(4, t0, "x", "route", "b", "lat", n("10")),
		rec(5, t0, "x", "route", "c"),
		rec(6, t0, "x", "route", "c"),
		rec(7, t0, "x", "route", "d", "lat", n("4")),
		rec(8, t0, "x", "route", "d", "lat", n("6")),
	}
}

func groupOrder(r Result) []any {
	var out []any
	for i := range r.Rows {
		out = append(out, r.Rows[i].Cells[0].V)
	}
	return out
}

func TestTableOrderByMetric(t *testing.T) {
	s := sch(routeF, latF)
	avg := []Aggregate{{Op: "avg", Field: "lat"}}
	cases := []struct {
		name  string
		order *Order
		want  []any
	}{
		{"default count desc, ties by key", nil, []any{"a", "c", "d", "b"}},
		{"count asc", &Order{Op: "count", Asc: true}, []any{"b", "c", "d", "a"}},
		{"avg desc, empty last", &Order{Op: "avg", Field: "lat"}, []any{"b", "d", "a", "c"}},
		{"avg asc, empty still last", &Order{Op: "avg", Field: "lat", Asc: true}, []any{"a", "d", "b", "c"}},
	}
	for _, c := range cases {
		res := run(t, orderRecs(), s, Query{View: "table", GroupBy: []string{"route"}, Aggregates: avg, Order: c.order})
		if got := groupOrder(res); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTableOrderAppliesBeforeLimit(t *testing.T) {
	s := sch(routeF, latF)
	res := run(t, orderRecs(), s, Query{View: "table", GroupBy: []string{"route"}, Limit: 1,
		Aggregates: []Aggregate{{Op: "max", Field: "lat"}}, Order: &Order{Op: "max", Field: "lat"}})
	if got := groupOrder(res); !reflect.DeepEqual(got, []any{"b"}) {
		t.Errorf("got %v, want [b]", got)
	}
}

func TestGraphOrderRanksSeries(t *testing.T) {
	s := sch(routeF, latF)
	res := run(t, orderRecs(), s, Query{View: "graph", GroupBy: []string{"route"}, Bucket: "1m", Range: Range{From: "all"},
		Aggregates: []Aggregate{{Op: "avg", Field: "lat"}}, Order: &Order{Op: "avg", Field: "lat", Asc: true}})
	var got []string
	for _, se := range res.Graph.Series {
		got = append(got, se.Label)
	}
	if want := []string{"a", "d", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("series %v, want %v", got, want)
	}
}

func TestValidateOrder(t *testing.T) {
	s := sch(routeF, latF)
	aggs := []Aggregate{{Op: "avg", Field: "lat"}}
	for _, o := range []*Order{{Op: "count"}, {Op: "count", Asc: true}, {Op: "avg", Field: "lat"}} {
		if err := (Query{Aggregates: aggs, Order: o}).Validate(s); err != nil {
			t.Errorf("%+v: %v", o, err)
		}
	}
	for _, o := range []*Order{{Op: "max", Field: "lat"}, {Op: "avg"}, {Op: "count", Field: "lat"}} {
		if err := (Query{Aggregates: aggs, Order: o}).Validate(s); err == nil {
			t.Errorf("%+v: want error", o)
		}
	}
}
