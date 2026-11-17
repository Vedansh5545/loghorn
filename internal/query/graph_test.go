package query

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/record"
)

var utcm8 = time.FixedZone("X", -8*3600)

func TestChooseBucketAuto(t *testing.T) {
	cases := []struct {
		span time.Duration
		want string
	}{
		{10 * time.Minute, "10s"}, {2 * time.Hour, "5m"}, {90 * time.Second, "1s"},
		{0, "1s"}, {30 * 24 * time.Hour, "1d"},
	}
	for _, c := range cases {
		_, name, err := chooseBucket("auto", c.span)
		if err != nil || name != c.want {
			t.Errorf("span %v: got %q %v want %q", c.span, name, err, c.want)
		}
	}
}

func TestChooseBucketTooMany(t *testing.T) {
	_, _, err := chooseBucket("1s", time.Hour)
	if err == nil || !strings.Contains(err.Error(), "pick a coarser granularity") {
		t.Fatalf("err = %v", err)
	}
}

func TestBucketStartAlign(t *testing.T) {
	at := time.Date(2026, 11, 17, 12, 34, 56, 0, time.UTC)
	if got := bucketStart(at, time.Minute, utcm8); !got.Equal(time.Date(2026, 11, 17, 12, 34, 0, 0, time.UTC)) {
		t.Errorf("1m = %v", got)
	}
	// 05:00 UTC is 21:00 the previous day at -8h; local midnight is 08:00 UTC.
	at = time.Date(2026, 11, 17, 5, 0, 0, 0, time.UTC)
	want := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	if got := bucketStart(at, 24*time.Hour, utcm8); !got.Equal(want) {
		t.Errorf("1d = %v want %v", got, want)
	}
	if got := bucketStart(at, 6*time.Hour, utcm8); !got.Equal(time.Date(2026, 11, 17, 2, 0, 0, 0, time.UTC)) {
		t.Errorf("6h = %v", got)
	}
}

func rv(g *Graph, i int) []string {
	var out []string
	for _, v := range g.Series[i].Values {
		if v == nil {
			out = append(out, "nil")
		} else {
			out = append(out, fmt.Sprint(*v))
		}
	}
	return out
}

func TestGraphCountFillsZero(t *testing.T) {
	s := sch(latF)
	recs := []record.Record{rec(1, t0, "x"), rec(2, t0.Add(3*time.Minute), "x")}
	g := run(t, recs, s, Query{View: "graph", Bucket: "1m", Range: Range{From: "all"}}).Graph
	if len(g.Series) != 1 || g.Series[0].Label != "all" || len(g.Starts) != 4 || !g.Starts[0].Equal(t0) {
		t.Fatalf("graph = %+v", g)
	}
	if fmt.Sprint(rv(g, 0)) != "[1 0 0 1]" {
		t.Errorf("values = %v", rv(g, 0))
	}
}

func TestGraphAvgGapIsNull(t *testing.T) {
	s := sch(latF)
	recs := []record.Record{rec(1, t0, "x", "lat", json.Number("2")), rec(2, t0.Add(3*time.Minute), "x", "lat", json.Number("4"))}
	q := Query{View: "graph", Bucket: "1m", Range: Range{From: "all"}, Aggregates: []Aggregate{{Op: "avg", Field: "lat"}}, Plot: &Aggregate{Op: "avg", Field: "lat"}}
	g := run(t, recs, s, q).Graph
	if fmt.Sprint(rv(g, 0)) != "[2 nil nil 4]" {
		t.Errorf("values = %v", rv(g, 0))
	}
}

func TestGraphTopTenPlusOther(t *testing.T) {
	s := sch(routeF)
	var recs []record.Record
	seq := 0
	for i := 0; i < 12; i++ {
		for j := 0; j <= i; j++ {
			seq++
			recs = append(recs, rec(seq, t0.Add(time.Duration(seq)*time.Second), "x", "route", fmt.Sprintf("r%02d", i)))
		}
	}
	g := run(t, recs, s, Query{View: "graph", GroupBy: []string{"route"}, Range: Range{From: "all"}}).Graph
	if len(g.Series) != 11 || g.Series[0].Label != "r11" || g.Series[0].Count != 12 {
		t.Fatalf("series = %d first %+v", len(g.Series), g.Series[0])
	}
	o := g.Series[10]
	if !o.Other || o.Label != "other" || o.Count != 3 { // groups with 1 and 2 records
		t.Errorf("other = %+v", o)
	}
}

func TestGraphRangeClippedToData(t *testing.T) {
	s := sch(latF)
	recs := []record.Record{rec(1, t0.Add(-time.Hour), "x"), rec(2, t0.Add(-time.Hour+2*time.Minute), "x")}
	g := run(t, recs, s, Query{View: "graph", Bucket: "1m", Range: Range{From: "all"}}).Graph
	want := t0.Add(-time.Hour + 2*time.Minute)
	if got := g.Starts[len(g.Starts)-1]; !got.Equal(want) || len(g.Starts) != 3 {
		t.Errorf("last start = %v of %d", got, len(g.Starts))
	}
}

func TestGraphNoTimedRecords(t *testing.T) {
	s := sch(latF)
	recs := []record.Record{rec(1, time.Time{}, "x"), rec(2, time.Time{}, "x")}
	res := run(t, recs, s, Query{View: "graph", Range: Range{From: "all"}})
	if res.Graph == nil || len(res.Graph.Series) != 0 || res.Matched != 2 {
		t.Fatalf("graph = %+v", res.Graph)
	}
	if _, err := json.Marshal(res); err != nil {
		t.Fatal(err)
	}
}

func TestGraphResultMarshals(t *testing.T) {
	s := sch(routeF, latF)
	recs := []record.Record{
		rec(1, t0, "x", "route", "a"),
		rec(2, t0.Add(2*time.Minute), "x", "route", "b", "lat", json.Number("3")),
	}
	q := Query{View: "graph", Bucket: "1m", GroupBy: []string{"route"}, Range: Range{From: "all"}, Aggregates: []Aggregate{{Op: "p95", Field: "lat"}}, Plot: &Aggregate{Op: "p95", Field: "lat"}}
	res := run(t, recs, s, q)
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "NaN") || !strings.Contains(string(b), "null") {
		t.Errorf("json = %s", b)
	}
}
