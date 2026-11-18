package query

import (
	"fmt"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/record"
)

// stairs has n routes; route rNN has NN+1 records, so r(n-1) is the largest.
func stairs(n int) []record.Record {
	var recs []record.Record
	seq := 0
	for i := 0; i < n; i++ {
		for j := 0; j <= i; j++ {
			seq++
			recs = append(recs, rec(seq, t0.Add(time.Duration(seq)*time.Second), "x", "route", fmt.Sprintf("r%02d", i)))
		}
	}
	return recs
}

func TestTableTop(t *testing.T) {
	s := sch(routeF)
	cases := []struct {
		name  string
		q     Query
		rows  int
		trunc bool
	}{
		{"default is 10", Query{}, 10, true},
		{"top 3", Query{Top: 3}, 3, true},
		{"top above groups", Query{Top: 20}, 12, false},
		{"limit when top unset", Query{Limit: 4}, 4, true},
		{"top wins over limit", Query{Top: 2, Limit: 4}, 2, true},
	}
	for _, c := range cases {
		c.q.View, c.q.GroupBy = "table", []string{"route"}
		res := run(t, stairs(12), s, c.q)
		if len(res.Rows) != c.rows || res.Truncated != c.trunc || res.Groups != 12 {
			t.Errorf("%s: rows=%d truncated=%v groups=%d", c.name, len(res.Rows), res.Truncated, res.Groups)
		}
	}
}

func TestGraphTop(t *testing.T) {
	s := sch(routeF)
	g := run(t, stairs(12), s, Query{View: "graph", GroupBy: []string{"route"}, Range: Range{From: "all"}, Top: 3}).Graph
	if len(g.Series) != 4 || g.Series[0].Label != "r11" || !g.Series[3].Other || g.Groups != 12 || g.Top != 3 {
		t.Fatalf("series=%d first=%q groups=%d top=%d", len(g.Series), g.Series[0].Label, g.Groups, g.Top)
	}
	if want := 1 + 2 + 3 + 4 + 5 + 6 + 7 + 8 + 9; g.Series[3].Count != want {
		t.Errorf("other count = %d, want %d", g.Series[3].Count, want)
	}
	// No "other" when every group fits.
	g = run(t, stairs(4), s, Query{View: "graph", GroupBy: []string{"route"}, Range: Range{From: "all"}}).Graph
	if len(g.Series) != 4 || g.Series[3].Other || g.Top != DefaultTop {
		t.Errorf("series=%d top=%d", len(g.Series), g.Top)
	}
}

func TestGraphTopCapped(t *testing.T) {
	s := sch(routeF)
	g := run(t, stairs(60), s, Query{View: "graph", GroupBy: []string{"route"}, Range: Range{From: "all"}, Top: 500}).Graph
	if g.Top != MaxGraphTop || len(g.Series) != MaxGraphTop+1 || g.Groups != 60 {
		t.Errorf("top=%d series=%d groups=%d", g.Top, len(g.Series), g.Groups)
	}
}

func TestValidateTop(t *testing.T) {
	s := sch(routeF)
	for _, top := range []int{-1, MaxLimit + 1} {
		if err := (Query{Top: top}).Validate(s); err == nil {
			t.Errorf("top %d: want error", top)
		}
	}
	if err := (Query{Top: 25}).Validate(s); err != nil {
		t.Error(err)
	}
}
