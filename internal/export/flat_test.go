package export

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/query"
	"github.com/jimbeveridge/loghorn/internal/schema"
)

const tricky = "a\tb\nc|d\"e,f\\g é"

var t0 = time.Date(2026, 11, 17, 12, 0, 0, 5, time.UTC)

func write(t *testing.T, f Format, res *query.Result) string {
	t.Helper()
	var b bytes.Buffer
	if err := Write(&b, f, res, schema.Schema{}); err != nil {
		t.Fatalf("Write %s: %v", f, err)
	}
	return b.String()
}

func samples() *query.Result {
	return &query.Result{
		View: "samples",
		Columns: []query.Column{
			{Path: "msg", Label: "Message", Type: schema.TString},
			{Path: "n", Type: schema.TInt},
			{Path: "x", Type: schema.TFloat},
			{Path: "ok", Type: schema.TBool},
			{Path: "at", Type: schema.TTimestamp},
			{Path: "j", Type: schema.TJSON},
		},
		Rows: []query.Row{
			{Cells: []query.Cell{{V: tricky}, {V: int64(7)}, {V: 0.15}, {V: true}, {V: t0.Format(time.RFC3339Nano)}, {V: `{"a":[1, 2]}`}}},
			{Cells: []query.Cell{{}, {V: "oops", Bad: true}, {V: math.Inf(-1)}, {V: false}, {}, {}}},
		},
		Matched: 2,
	}
}

func TestFlatSamples(t *testing.T) {
	res := samples()

	recs, err := csv.NewReader(strings.NewReader(write(t, CSV, res))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"msg", "n", "x", "ok", "at", "j"},
		{tricky, "7", "0.15", "true", "2026-11-17T12:00:00.000000005Z", `{"a":[1,2]}`},
		{"", "", "-Infinity", "false", "", ""},
	}
	if len(recs) != len(want) {
		t.Fatalf("csv rows = %q", recs)
	}
	for i := range want {
		if strings.Join(recs[i], "\x00") != strings.Join(want[i], "\x00") {
			t.Errorf("csv row %d = %q; want %q", i, recs[i], want[i])
		}
	}

	tsv := strings.Split(write(t, TSV, res), "\n")
	if tsv[0] != "msg\tn\tx\tok\tat\tj" {
		t.Errorf("tsv header = %q", tsv[0])
	}
	if want := `a\tb\nc|d"e,f\\g é` + "\t7\t0.15\ttrue\t2026-11-17T12:00:00.000000005Z\t" + `{"a":[1,2]}`; tsv[1] != want {
		t.Errorf("tsv row = %q; want %q", tsv[1], want)
	}
	if want := "\t\t-Infinity\tfalse\t\t"; tsv[2] != want {
		t.Errorf("tsv null row = %q; want %q", tsv[2], want)
	}
	if len(tsv) != 4 || tsv[3] != "" {
		t.Errorf("tsv lines = %q", tsv)
	}

	md := strings.Split(write(t, MD, res), "\n")
	if md[0] != "| msg | n | x | ok | at | j |" || md[1] != "| --- | --- | --- | --- | --- | --- |" {
		t.Errorf("md head = %q", md[:2])
	}
	if want := "| a\tb<br>c\\|d\"e,f\\g é | 7 | 0.15 | true | 2026-11-17T12:00:00.000000005Z | {\"a\":[1,2]} |"; md[2] != want {
		t.Errorf("md row = %q; want %q", md[2], want)
	}
	if want := "|  |  | -Infinity | false |  |  |"; md[3] != want {
		t.Errorf("md null row = %q; want %q", md[3], want)
	}

	lines := strings.Split(strings.TrimSuffix(write(t, JSONL, res), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("jsonl = %q", lines)
	}
	if want := `{"msg":"a\tb\nc|d\"e,f\\g é","n":7,"x":0.15,"ok":true,"at":"2026-11-17T12:00:00.000000005Z","j":{"a":[1,2]}}`; lines[0] != want {
		t.Errorf("jsonl = %s; want %s", lines[0], want)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &m); err != nil || m["msg"] != tricky {
		t.Errorf("jsonl msg = %q, %v", m["msg"], err)
	}
	if want := `{"msg":null,"n":null,"x":"-Infinity","ok":false,"at":null,"j":null}`; lines[1] != want {
		t.Errorf("jsonl = %s; want %s", lines[1], want)
	}
}

func TestFlatNumbers(t *testing.T) {
	res := &query.Result{
		View:    "samples",
		Columns: []query.Column{{Path: "n", Type: schema.TInt}, {Path: "x", Type: schema.TFloat}, {Path: "y", Type: schema.TFloat}, {Path: "z", Type: schema.TFloat}},
		Rows:    []query.Row{{Cells: []query.Cell{{V: "12345678901234567890"}, {V: math.NaN()}, {V: math.Inf(1)}, {V: "NaN"}}}},
	}
	if got := write(t, CSV, res); got != "n,x,y,z\n12345678901234567890,NaN,Infinity,NaN\n" {
		t.Errorf("csv = %q", got)
	}
	if got := write(t, JSONL, res); got != `{"n":12345678901234567890,"x":"NaN","y":"Infinity","z":"NaN"}`+"\n" {
		t.Errorf("jsonl = %q", got)
	}
}

func tableResult() *query.Result {
	return &query.Result{
		View:   "table",
		Groups: 2,
		Columns: []query.Column{
			{Path: "method", Label: "Method", Group: true, Type: schema.TString},
			{Path: "status", Group: true, Type: schema.TInt},
			{Path: "count", Label: "count", Type: schema.TInt},
			{Path: "p95(duration_ms)", Label: "p95 Duration", Type: schema.TFloat},
			{Path: "error_rate", Label: "error rate", Type: schema.TFloat},
		},
		Rows: []query.Row{
			{Cells: []query.Cell{{V: "GET"}, {V: int64(200)}, {V: int64(3)}, {V: 23.84}, {V: 0.5}}},
			{Cells: []query.Cell{{}, {V: int64(500)}, {V: int64(1)}, {}, {V: 0.0}}},
		},
	}
}

func TestFlatTable(t *testing.T) {
	got := write(t, CSV, tableResult())
	want := "method,status,count,p95(duration_ms),error_rate\nGET,200,3,23.84,0.5\n,500,1,,0\n"
	if got != want {
		t.Errorf("csv = %q; want %q", got, want)
	}
}

func f64(v float64) *float64 { return &v }

func graphResult(series ...query.Series) *query.Result {
	return &query.Result{
		View: "graph",
		Graph: &query.Graph{
			Bucket: "1m",
			Starts: []time.Time{t0, t0.Add(time.Minute), t0.Add(2 * time.Minute)},
			Metric: query.Column{Path: "count", Type: schema.TInt},
			Series: series,
		},
	}
}

func TestFlatGraphWide(t *testing.T) {
	res := graphResult(
		query.Series{Label: "GET", Group: []string{"GET"}, Values: []*float64{f64(1), nil, f64(2.5)}},
		query.Series{Label: "other", Other: true, Values: []*float64{f64(4), f64(0), nil}},
	)
	want := "time,GET,other\n" +
		"2026-11-17T12:00:00.000000005Z,1,4\n" +
		"2026-11-17T12:01:00.000000005Z,,0\n" +
		"2026-11-17T12:02:00.000000005Z,2.5,\n"
	if got := write(t, CSV, res); got != want {
		t.Errorf("csv = %q; want %q", got, want)
	}
	if got := write(t, JSONL, res); !strings.HasPrefix(got, `{"time":"2026-11-17T12:00:00.000000005Z","GET":1,"other":4}`+"\n"+`{"time":"2026-11-17T12:01:00.000000005Z","GET":null,"other":0}`) {
		t.Errorf("jsonl = %q", got)
	}

	// No group-by: the single column is the plot metric's name.
	res = graphResult(query.Series{Label: "all", Values: []*float64{f64(1), f64(2), nil}})
	if got := write(t, CSV, res); !strings.HasPrefix(got, "time,count\n") {
		t.Errorf("csv = %q", got)
	}
	res.Graph.Metric = query.Column{Path: "p95(duration_ms)", Label: "p95 Duration", Type: schema.TFloat}
	if got := write(t, CSV, res); !strings.HasPrefix(got, "time,p95(duration_ms)\n") {
		t.Errorf("csv = %q", got)
	}

	// Duplicate labels stay unique.
	res = graphResult(
		query.Series{Label: "a", Group: []string{"a"}, Values: []*float64{nil, nil, nil}},
		query.Series{Label: "a", Group: []string{"a"}, Values: []*float64{nil, nil, nil}},
		query.Series{Label: "a", Group: []string{"a"}, Values: []*float64{nil, nil, nil}},
		query.Series{Label: "time", Group: []string{"time"}, Values: []*float64{nil, nil, nil}},
	)
	if got := write(t, CSV, res); !strings.HasPrefix(got, "time,a,a (2),a (3),time (2)\n") {
		t.Errorf("csv = %q", got)
	}
}

func heatResult() *query.Result {
	return &query.Result{
		View: "heatmap",
		Heatmap: &query.Heatmap{
			Bucket: "1m",
			Starts: []time.Time{t0, t0.Add(time.Minute)},
			Bands:  []query.Band{{Lo: 0.5, Hi: 1}, {Lo: 1, Hi: 2.15}},
			Counts: [][]int{{0, 3}, {2, 0}},
		},
	}
}

func TestFlatHeatmap(t *testing.T) {
	want := "time,<1 ms,1-2.15 ms\n2026-11-17T12:00:00.000000005Z,0,3\n2026-11-17T12:01:00.000000005Z,2,0\n"
	if got := write(t, CSV, heatResult()); got != want {
		t.Errorf("csv = %q; want %q", got, want)
	}
	want = `{"time":"2026-11-17T12:00:00.000000005Z","band":"1-2.15 ms","lo":1,"hi":2.15,"count":3}` + "\n" +
		`{"time":"2026-11-17T12:01:00.000000005Z","band":"<1 ms","hi":1,"count":2}` + "\n"
	if got := write(t, JSONL, heatResult()); got != want {
		t.Errorf("jsonl = %q; want %q", got, want)
	}
}

func TestBandName(t *testing.T) {
	if got := BandName(query.Band{Lo: 0.1, Hi: 1}, true); got != "<1 ms" {
		t.Errorf("lowest = %q", got)
	}
	if got := BandName(query.Band{Lo: 1, Hi: 2.15}, false); got != "1-2.15 ms" {
		t.Errorf("band = %q", got)
	}
}

func TestFlatEmpty(t *testing.T) {
	cases := map[string]struct {
		res            *query.Result
		csv, md, jsonl string
	}{
		"rows": {
			res: &query.Result{View: "samples", Columns: []query.Column{{Path: "a"}, {Path: "b"}}},
			csv: "a,b\n", md: "| a | b |\n| --- | --- |\n",
		},
		"nil columns": {res: &query.Result{View: "samples"}},
		"table": {
			res: &query.Result{View: "table", Columns: []query.Column{{Path: "count"}}},
			csv: "count\n", md: "| count |\n| --- |\n",
		},
		"graph": {
			res: &query.Result{View: "graph", Graph: &query.Graph{Starts: []time.Time{}, Metric: query.Column{Path: "count"}, Series: []query.Series{}}},
			csv: "time,count\n", md: "| time | count |\n| --- | --- |\n",
		},
		"graph nil": {
			res: &query.Result{View: "graph"}, csv: "time\n", md: "| time |\n| --- |\n",
		},
		"heatmap": {
			res: &query.Result{View: "heatmap", Heatmap: &query.Heatmap{}}, csv: "time\n", md: "| time |\n| --- |\n",
		},
	}
	for name, c := range cases {
		if got := write(t, CSV, c.res); got != c.csv {
			t.Errorf("%s csv = %q; want %q", name, got, c.csv)
		}
		if got := write(t, TSV, c.res); got != strings.ReplaceAll(c.csv, ",", "\t") {
			t.Errorf("%s tsv = %q", name, got)
		}
		if got := write(t, MD, c.res); got != c.md {
			t.Errorf("%s md = %q; want %q", name, got, c.md)
		}
		if got := write(t, JSONL, c.res); got != "" {
			t.Errorf("%s jsonl = %q", name, got)
		}
	}
}

func TestNote(t *testing.T) {
	samples := &query.Result{View: "samples", Matched: 250, Rows: make([]query.Row, 100), Truncated: true}
	if n := Note(samples, query.Query{}); !strings.Contains(n, "250") || !strings.Contains(n, "100") {
		t.Errorf("samples note = %q", n)
	}
	samples.Truncated, samples.Matched = false, 100
	if n := Note(samples, query.Query{}); n != "" {
		t.Errorf("samples note = %q", n)
	}

	tbl := tableResult()
	tbl.Groups = 5
	if n := Note(tbl, query.Query{View: "table"}); !strings.Contains(n, "5") || !strings.Contains(n, "2") {
		t.Errorf("table note = %q", n)
	}
	tbl.Groups = 2
	if n := Note(tbl, query.Query{View: "table"}); n != "" {
		t.Errorf("table note = %q", n)
	}

	g := graphResult(query.Series{Label: "GET"}, query.Series{Label: "other", Other: true})
	g.Graph.Groups, g.Graph.Top = 12, 1
	if n := Note(g, query.Query{View: "graph"}); !strings.Contains(n, "other") {
		t.Errorf("graph note = %q", n)
	}
	g = graphResult(query.Series{Label: "GET"})
	if n := Note(g, query.Query{View: "graph"}); n != "" {
		t.Errorf("graph note = %q", n)
	}
	if n := Note(heatResult(), query.Query{}); n != "" {
		t.Errorf("heatmap note = %q", n)
	}
}

func TestFormats(t *testing.T) {
	for _, s := range []string{"csv", "tsv", "md", "json", "jsonl", "yaml", "toon", "sql"} {
		f, err := ParseFormat(s)
		if err != nil || string(f) != s {
			t.Errorf("ParseFormat(%q) = %q, %v", s, f, err)
		}
		if !strings.HasSuffix(f.ContentType(), "; charset=utf-8") {
			t.Errorf("%s content type = %q", f, f.ContentType())
		}
	}
	if _, err := ParseFormat("bogus"); err == nil {
		t.Error("bogus: want error")
	}
	if CSV.ContentType() != "text/csv; charset=utf-8" || TOON.ContentType() != "text/plain; charset=utf-8" {
		t.Error("content types")
	}
	paths := map[string]Format{"a.csv": CSV, "x/b.TSV": TSV, "c.md": MD, "d.json": JSON, "e.jsonl": JSONL, "f.yaml": YAML, "g.yml": YAML, "h.toon": TOON, "i.sql": SQL}
	for p, want := range paths {
		if f, ok := FormatFromPath(p); !ok || f != want {
			t.Errorf("FormatFromPath(%q) = %q, %v", p, f, ok)
		}
	}
	if _, ok := FormatFromPath("x.txt"); ok {
		t.Error("x.txt: want false")
	}
	var b bytes.Buffer
	if err := Write(&b, SQL, samples(), schema.Schema{}); err == nil {
		t.Error("sql: want error from Write")
	}
}
