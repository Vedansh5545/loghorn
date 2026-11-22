package query

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/schema"
)

// hspan is a span of stream "a" with the given duration (nil: none).
func hspan(seq int, at time.Time, dur any, kv ...any) record.Record {
	if dur != nil {
		kv = append(kv, "duration_ms", dur)
	}
	r := rec(seq, at, "", kv...)
	r.Source = record.SourceOtel
	return r
}

func hsch(t schema.Type) schema.Schema {
	return sch(schema.Field{Path: "duration_ms", Type: t}, schema.Field{Path: "name", Type: schema.TString})
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-4 }

// bandOf is the index of the band holding d, or -1.
func bandOf(h *Heatmap, d float64) int {
	for i, b := range h.Bands {
		if b.Lo <= d && d < b.Hi {
			return i
		}
	}
	return -1
}

func heatmap(t *testing.T, recs []record.Record, s schema.Schema, q Query) *Heatmap {
	t.Helper()
	q.Source, q.Stream, q.View = record.SourceOtel, record.AllStreams, "heatmap"
	res := run(t, recs, s, q)
	if res.Heatmap == nil {
		t.Fatalf("no heatmap in %+v", res)
	}
	if len(res.Heatmap.Counts) != len(res.Heatmap.Starts) {
		t.Fatalf("%d count rows for %d starts", len(res.Heatmap.Counts), len(res.Heatmap.Starts))
	}
	for i, row := range res.Heatmap.Counts {
		if len(row) != len(res.Heatmap.Bands) {
			t.Fatalf("row %d has %d counts for %d bands", i, len(row), len(res.Heatmap.Bands))
		}
	}
	return res.Heatmap
}

func TestBandsEdges(t *testing.T) {
	b := bands(1.2, 30)
	if len(b) != 10 || b[0].Lo != 1 || b[9].Hi != 100 {
		t.Fatalf("bands = %+v", b)
	}
	if !near(b[1].Lo, 1.5849) || !near(b[2].Lo, 2.5119) || !near(b[2].Hi, 3.9811) {
		t.Errorf("bands = %+v", b)
	}
	for i := 1; i < len(b); i++ {
		if b[i].Lo != b[i-1].Hi {
			t.Errorf("band %d Lo %v != previous Hi %v", i, b[i].Lo, b[i-1].Hi)
		}
	}
}

func TestBandsSingleDecadeMinimum(t *testing.T) {
	b := bands(5, 5)
	if len(b) != 5 || b[0].Lo != 1 || b[4].Hi != 10 {
		t.Fatalf("bands = %+v", b)
	}
}

func TestBandsExactDecades(t *testing.T) {
	// A maximum exactly on a decade edge needs a band above it; a minimum
	// exactly on one starts there.
	b := bands(1000, 1000)
	if b[0].Lo != 1000 || b[len(b)-1].Hi != 10000 {
		t.Errorf("bands(1000, 1000) = %v .. %v", b[0].Lo, b[len(b)-1].Hi)
	}
	b = bands(1, 100)
	if b[0].Lo != 1 || b[len(b)-1].Hi != 1000 || len(b) != 15 {
		t.Errorf("bands(1, 100) = %d bands %v .. %v", len(b), b[0].Lo, b[len(b)-1].Hi)
	}
	b = bands(0.001, 0.001)
	if b[0].Lo != 0.001 || len(b) != 5 {
		t.Errorf("bands(0.001, 0.001) = %+v", b)
	}
	if b := bands(0, -1); b[0].Lo != 1 || b[len(b)-1].Hi != 10 {
		t.Errorf("bands(0, -1) = %+v", b)
	}
}

func TestHeatmapCounts(t *testing.T) {
	recs := []record.Record{
		hspan(1, t0, json.Number("2")),
		hspan(2, t0.Add(30*time.Second), json.Number("2")),
		hspan(3, t0.Add(90*time.Second), json.Number("200")),
	}
	h := heatmap(t, recs, hsch(schema.TFloat), Query{Bucket: "1m"})
	if h.Bucket != "1m" || h.BucketSeconds != 60 || len(h.Starts) != 2 || !h.Starts[0].Equal(t0) {
		t.Fatalf("axis = %s %v %v", h.Bucket, h.BucketSeconds, h.Starts)
	}
	b2, b200 := bandOf(h, 2), bandOf(h, 200)
	if b2 < 0 || b200 < 0 || h.Counts[0][b2] != 2 || h.Counts[1][b200] != 1 {
		t.Errorf("counts = %v (bands %d, %d)", h.Counts, b2, b200)
	}
	if h.Max != 2 || h.Total != 3 || h.Skipped != 0 {
		t.Errorf("max %d total %d skipped %d", h.Max, h.Total, h.Skipped)
	}
	if h.Bands[0].Lo != 1 || h.Bands[len(h.Bands)-1].Hi != 1000 {
		t.Errorf("bands %v .. %v", h.Bands[0].Lo, h.Bands[len(h.Bands)-1].Hi)
	}
}

func TestHeatmapSkippedAndNonPositive(t *testing.T) {
	recs := []record.Record{
		hspan(1, t0, nil),
		hspan(2, t0, json.Number("0")),
		hspan(3, t0, json.Number("-1")),
		hspan(4, time.Time{}, json.Number("5")),
	}
	h := heatmap(t, recs, hsch(schema.TFloat), Query{})
	if h.Skipped != 2 || h.Total != 2 || len(h.Starts) != 1 || h.Counts[0][0] != 2 {
		t.Errorf("skipped %d total %d counts %v", h.Skipped, h.Total, h.Counts)
	}
}

func TestHeatmapEmpty(t *testing.T) {
	h := heatmap(t, nil, hsch(schema.TFloat), Query{})
	if h.Total != 0 || len(h.Starts) != 0 || h.Starts == nil || h.Counts == nil || len(h.Bands) == 0 {
		t.Errorf("empty heatmap = %+v", h)
	}
	b, _ := json.Marshal(h)
	if want := `"starts":[]`; !strings.Contains(string(b), want) || !strings.Contains(string(b), `"counts":[]`) {
		t.Errorf("json = %s", b)
	}
}

// Review Focus 3: an int- or string-typed duration_ms still bins, and a
// cell over it still selects.
func TestHeatmapIntField(t *testing.T) {
	for _, typ := range []schema.Type{schema.TInt, schema.TString} {
		recs := []record.Record{
			hspan(1, t0, json.Number("3")),
			hspan(2, t0, json.Number("40")),
			hspan(3, t0, json.Number("2.5")),
		}
		s := hsch(typ)
		h := heatmap(t, recs, s, Query{})
		if h.Total != 3 || h.Skipped != 0 || h.Counts[0][bandOf(h, 3)] != 1 || h.Counts[0][bandOf(h, 40)] != 1 {
			t.Errorf("%s: total %d skipped %d counts %v", typ, h.Total, h.Skipped, h.Counts)
		}
		b := h.Bands[bandOf(h, 3)]
		res := run(t, recs, s, Query{Source: "otel", Stream: "*",
			Cell: &HeatCell{From: t0, To: t0.Add(time.Second), Lo: &b.Lo, Hi: b.Hi}})
		if got := seqs(res); len(got) != 1 || got[0] != 1 {
			t.Errorf("%s: cell rows = %v", typ, got)
		}
	}
}

// Review Focus 1: a span on the next bucket's start belongs to that bucket
// only.
func TestCellHalfOpen(t *testing.T) {
	to := t0.Add(time.Minute)
	recs := []record.Record{
		hspan(1, t0, json.Number("2")),
		hspan(2, to, json.Number("2")),
		hspan(3, t0.Add(-time.Nanosecond), json.Number("2")),
	}
	res := run(t, recs, hsch(schema.TFloat), Query{Source: "otel", Stream: "*",
		Cell: &HeatCell{From: t0, To: to, Hi: 10}})
	if got := seqs(res); len(got) != 1 || got[0] != 1 || res.Matched != 1 {
		t.Errorf("rows = %v matched %d", got, res.Matched)
	}
}

func TestCellBandBounds(t *testing.T) {
	recs := []record.Record{
		hspan(1, t0, json.Number("1.5849")),
		hspan(2, t0, json.Number("2.0")),
		hspan(3, t0, json.Number("2.5119")),
		hspan(4, t0, json.Number("0")),
		hspan(5, t0, json.Number("-1")),
		hspan(6, t0, nil),
	}
	lo := 1.5849
	q := Query{Source: "otel", Stream: "*", Sort: Sort{Field: "timestamp"},
		Cell: &HeatCell{From: t0, To: t0.Add(time.Minute), Lo: &lo, Hi: 2.5119}}
	if got := seqs(run(t, recs, hsch(schema.TFloat), q)); len(got) != 2 || got[0]+got[1] != 3 {
		t.Errorf("[1.5849, 2.5119) rows = %v", got)
	}
	q.Cell = &HeatCell{From: t0, To: t0.Add(time.Minute), Hi: 1.5849}
	if got := seqs(run(t, recs, hsch(schema.TFloat), q)); len(got) != 2 || got[0]+got[1] != 9 {
		t.Errorf("[, 1.5849) rows = %v", got)
	}
}

func TestCellAfterFilters(t *testing.T) {
	recs := []record.Record{
		hspan(1, t0, json.Number("2"), "name", "a"),
		hspan(2, t0, json.Number("2"), "name", "b"),
		hspan(3, t0.Add(time.Hour), json.Number("2"), "name", "a"),
	}
	q := Query{Source: "otel", Stream: "*", Filters: []Filter{{Field: "name", Op: "=", Values: []string{"a"}}},
		Cell: &HeatCell{From: t0, To: t0.Add(time.Minute), Hi: 10}}
	if got := seqs(run(t, recs, hsch(schema.TFloat), q)); len(got) != 1 || got[0] != 1 {
		t.Errorf("rows = %v", got)
	}
}

// Every cell's selection lists exactly the cell's count, including spans
// on bucket starts and band edges (Review Focus 1 and 2).
func TestHeatmapCellsMatchCounts(t *testing.T) {
	durs := []any{json.Number("1"), json.Number("1.5848931924611136"), json.Number("10"),
		json.Number("0"), json.Number("99.99"), json.Number("100"), json.Number("3"), json.Number("0.5")}
	var recs []record.Record
	for i := range 40 {
		at := t0.Add(time.Duration(i) * 15 * time.Second) // every 4th on a minute
		if i%7 == 3 {
			at = at.Add(-time.Nanosecond)
		}
		recs = append(recs, hspan(i+1, at, durs[i%len(durs)]))
	}
	s := hsch(schema.TFloat)
	from := t0.Add(20 * time.Second) // mid-bucket
	q := Query{Bucket: "1m", Range: Range{From: from.Format(time.RFC3339Nano), To: t0.Add(8 * time.Minute).Format(time.RFC3339Nano)}}
	h := heatmap(t, recs, s, q)
	// The page's view of the grid: through JSON.
	b, _ := json.Marshal(h)
	var hp Heatmap
	if err := json.Unmarshal(b, &hp); err != nil {
		t.Fatal(err)
	}
	size := time.Duration(hp.BucketSeconds * float64(time.Second))
	sum := 0
	for ti, st := range hp.Starts {
		cf := st
		if cf.Before(from) {
			cf = from
		}
		for bi, band := range hp.Bands {
			c := &HeatCell{From: cf, To: st.Add(size), Hi: band.Hi}
			if bi > 0 {
				lo := band.Lo
				c.Lo = &lo
			}
			cq := q
			cq.Source, cq.Stream, cq.View, cq.Cell, cq.Limit = "otel", "*", "", c, MaxLimit
			res := run(t, recs, s, cq)
			if res.Matched != hp.Counts[ti][bi] {
				t.Errorf("cell %d,%d: %d rows, count %d", ti, bi, res.Matched, hp.Counts[ti][bi])
			}
			sum += res.Matched
		}
	}
	if sum != hp.Total || hp.Total == 0 {
		t.Errorf("cells sum to %d, total %d", sum, hp.Total)
	}
}

func TestHeatmapRejectedForLogs(t *testing.T) {
	for _, src := range []string{"", "log"} {
		if err := (Query{View: "heatmap", Source: src}).Validate(hsch(schema.TFloat)); err == nil {
			t.Errorf("source %q: heatmap accepted", src)
		}
	}
	if err := (Query{View: "heatmap", Source: "otel"}).Validate(hsch(schema.TFloat)); err != nil {
		t.Errorf("otel heatmap: %v", err)
	}
}

func TestCellValidate(t *testing.T) {
	lo := 5.0
	bad := []*HeatCell{
		{From: t0, To: t0, Hi: 10},
		{From: t0, To: t0.Add(time.Minute), Lo: &lo, Hi: 5},
	}
	for _, c := range bad {
		if err := (Query{Source: "otel", Cell: c}).Validate(hsch(schema.TFloat)); err == nil {
			t.Errorf("cell %+v accepted", c)
		}
	}
}
