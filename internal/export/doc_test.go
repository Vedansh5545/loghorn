package export

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/toon-format/toon-go"
	"gopkg.in/yaml.v3"

	"github.com/jimbeveridge/loghorn/internal/query"
	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/schema"
)

var docFormats = []Format{JSON, YAML, TOON}

func writeWith(t *testing.T, f Format, res *query.Result, s schema.Schema) string {
	t.Helper()
	var b bytes.Buffer
	if err := Write(&b, f, res, s); err != nil {
		t.Fatalf("Write %s: %v", f, err)
	}
	return b.String()
}

// decode parses a document with the format's own decoder.
func decode(t *testing.T, f Format, text string) map[string]any {
	t.Helper()
	var v any
	var err error
	switch f {
	case JSON:
		dec := json.NewDecoder(strings.NewReader(text))
		dec.UseNumber()
		err = dec.Decode(&v)
	case YAML:
		err = yaml.Unmarshal([]byte(text), &v)
	case TOON:
		v, err = toon.Decode([]byte(text))
	}
	if err != nil {
		t.Fatalf("decode %s: %v\n%s", f, err, text)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s document is %T, want an object\n%s", f, v, text)
	}
	return m
}

// docOf writes res in f and decodes it.
func docOf(t *testing.T, f Format, res *query.Result, s schema.Schema) map[string]any {
	t.Helper()
	return decode(t, f, writeWith(t, f, res, s))
}

// numOf is a decoded number as float64, whatever type the decoder chose.
func numOf(t *testing.T, v any) float64 {
	t.Helper()
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			t.Fatal(err)
		}
		return f
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case uint64:
		return float64(x)
	case float64:
		return x
	}
	t.Fatalf("%v (%T) is not a number", v, v)
	return 0
}

func isNum(v any) bool {
	switch v.(type) {
	case json.Number, int, int64, uint64, float64:
		return true
	}
	return false
}

func arr(t *testing.T, v any) []any {
	t.Helper()
	a, ok := v.([]any)
	if !ok {
		t.Fatalf("%v (%T) is not an array", v, v)
	}
	return a
}

func obj(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%v (%T) is not an object", v, v)
	}
	return m
}

func colByName(t *testing.T, doc map[string]any, name string) map[string]any {
	t.Helper()
	for _, c := range arr(t, doc["columns"]) {
		if m := obj(t, c); m["name"] == name {
			return m
		}
	}
	t.Fatalf("no column %q in %v", name, doc["columns"])
	return nil
}

func docTable() *query.Result {
	return &query.Result{
		View: "table",
		Columns: []query.Column{
			{Path: "method", Type: schema.TString, Group: true},
			{Path: "status", Type: schema.TInt, Group: true},
			{Path: "count", Type: schema.TInt},
			{Path: "p95(duration_ms)", Type: schema.TFloat},
			{Path: "error_rate", Type: schema.TFloat},
		},
		Rows: []query.Row{
			{Cells: []query.Cell{{V: "GET"}, {V: int64(200)}, {V: int64(3)}, {V: 23.84}, {V: 0.0}}},
			{Cells: []query.Cell{{V: "POST"}, {V: int64(200)}, {V: int64(2)}, {V: 5.0}, {V: 0.5}}},
			{Cells: []query.Cell{{V: "GET"}, {V: int64(500)}, {V: int64(1)}, {}, {V: 1.0}}},
		},
		Matched: 6, StreamTotal: 10, Groups: 3,
		From: t0, To: t0.Add(time.Hour),
	}
}

func docGraph() *query.Result {
	starts := []time.Time{t0, t0.Add(time.Minute), t0.Add(2 * time.Minute)}
	return &query.Result{
		View: "graph",
		Graph: &query.Graph{
			Bucket: "1m", BucketSeconds: 60, Starts: starts,
			Metric:  query.Column{Path: "count", Type: schema.TInt},
			GroupBy: []string{"method", "status"},
			Groups:  4, Top: 3,
			Series: []query.Series{
				{Label: "GET · 200", Group: []string{"GET", "200"}, Count: 4, Values: []*float64{f64(1), nil, f64(3)}},
				{Label: "POST · 200", Group: []string{"POST", "200"}, Count: 2, Values: []*float64{f64(2), nil, nil}},
				{Label: "GET · 500", Group: []string{"GET", "500"}, Count: 1, Values: []*float64{nil, f64(1), nil}},
				{Label: "other", Group: []string{}, Other: true, Count: 1, Values: []*float64{nil, nil, f64(1)}},
			},
		},
		Matched: 8,
	}
}

func docHeatmap() *query.Result {
	return &query.Result{
		View: "heatmap",
		Heatmap: &query.Heatmap{
			Bucket: "1m", BucketSeconds: 60,
			Starts: []time.Time{t0, t0.Add(time.Minute), t0.Add(2 * time.Minute)},
			Bands:  []query.Band{{Lo: 0, Hi: 1}, {Lo: 1, Hi: 10}, {Lo: 10, Hi: 100}},
			Counts: [][]int{{1, 0, 0}, {0, 0, 0}, {0, 2, 3}},
			Max:    3, Total: 6, Skipped: 1,
		},
		Matched: 7,
	}
}

var statusSchema = schema.Schema{Fields: []schema.Field{
	{Path: "method", Type: schema.TString},
	{Path: "status", Type: schema.TInt},
}}

func TestDocColumnsFirst(t *testing.T) {
	data := map[string][]string{
		"samples": {"rows"},
		"table":   {"groups"},
		"graph":   {"bucket", "starts", "series"},
		"heatmap": {"bucket", "bands", "max", "total", "skipped", "buckets"},
	}
	results := map[string]*query.Result{
		"samples": samples(), "table": docTable(), "graph": docGraph(), "heatmap": docHeatmap(),
	}
	for view, res := range results {
		keys := append([]string{"view", "columns", "matched", "streamTotal", "truncated", "from", "to"}, data[view]...)
		for _, f := range docFormats {
			out := writeWith(t, f, res, statusSchema)
			last := -1
			for _, k := range keys {
				var re *regexp.Regexp
				if f == JSON {
					re = regexp.MustCompile(`(?m)^  "` + regexp.QuoteMeta(k) + `":`)
				} else {
					re = regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(k) + `[:\[]`)
				}
				loc := re.FindStringIndex(out)
				if loc == nil {
					t.Fatalf("%s %s: no top-level key %q in\n%s", view, f, k, out)
				}
				if loc[0] <= last {
					t.Errorf("%s %s: key %q out of order in\n%s", view, f, k, out)
				}
				last = loc[0]
			}
			doc := decode(t, f, out)
			if doc["view"] != view {
				t.Errorf("%s %s: view = %v", view, f, doc["view"])
			}
		}
	}
}

func TestDocColumnDesc(t *testing.T) {
	s := schema.Schema{Source: record.SourceOtel}
	res := &query.Result{
		View: "samples",
		Columns: []query.Column{
			{Path: "duration_ms", Type: schema.TDuration, Unit: "ms"},
			{Path: "msg", Type: schema.TString},
			{Path: "n", Type: schema.TInt},
			{Path: "attributes.name", Type: schema.TString},
			{Path: "at", Type: schema.TTimestamp, Unit: "ms"},
		},
		Rows: []query.Row{
			{Cells: []query.Cell{{V: 0.087}, {V: "a"}, {V: "x", Bad: true}, {V: "n1"}, {V: t0.Format(time.RFC3339Nano)}}},
			{Cells: []query.Cell{{V: 1.5}, {}, {V: "y", Bad: true}, {V: "n2"}, {V: t0.Format(time.RFC3339Nano)}}},
			{Cells: []query.Cell{{V: 0.25}, {V: "c"}, {V: int64(4)}, {V: "n3"}, {V: t0.Format(time.RFC3339Nano)}}},
		},
	}
	cols := Columns(res, s)
	want := []ColumnDesc{
		{Name: "duration_ms", Type: "DOUBLE", Unit: "ms"},
		{Name: "msg", Type: "VARCHAR", Nullable: true},
		{Name: "n", Type: "BIGINT", Nullable: true, Mismatched: 2},
		{Name: "attributes.name", Type: "VARCHAR", OTelName: "name"},
		{Name: "at", Type: "TIMESTAMPTZ"},
	}
	if fmt.Sprint(cols) != fmt.Sprint(want) {
		t.Errorf("Columns = %+v\nwant %+v", cols, want)
	}
	for _, f := range docFormats {
		out := writeWith(t, f, res, s)
		doc := decode(t, f, out)
		d := colByName(t, doc, "duration_ms")
		if d["type"] != "DOUBLE" || d["unit"] != "ms" || d["nullable"] != false {
			t.Errorf("%s duration column = %v", f, d)
		}
		if _, ok := d["otel_name"]; ok {
			t.Errorf("%s: non-collided column has otel_name: %v", f, d)
		}
		if _, ok := d["mismatched"]; ok {
			t.Errorf("%s: mismatched 0 not omitted: %v", f, d)
		}
		if c := colByName(t, doc, "msg"); c["nullable"] != true {
			t.Errorf("%s msg column = %v", f, c)
		}
		if c := colByName(t, doc, "n"); numOf(t, c["mismatched"]) != 2 {
			t.Errorf("%s n column = %v", f, c)
		}
		if c := colByName(t, doc, "attributes.name"); c["otel_name"] != "name" {
			t.Errorf("%s attributes.name column = %v", f, c)
		}
		if c := colByName(t, doc, "at"); c["unit"] != nil {
			t.Errorf("%s: timestamp has a unit: %v", f, c)
		}
		rows := arr(t, doc["rows"])
		if len(rows) != 3 {
			t.Fatalf("%s rows = %v", f, rows)
		}
		r0, r1 := obj(t, rows[0]), obj(t, rows[1])
		if numOf(t, r0["duration_ms"]) != 87 || numOf(t, r1["duration_ms"]) != 1500 {
			t.Errorf("%s durations = %v, %v; want ms", f, r0["duration_ms"], r1["duration_ms"])
		}
		if v, ok := r0["n"]; !ok || v != nil {
			t.Errorf("%s bad cell = %v (present %v); want null", f, v, ok)
		}
		if v, ok := r1["msg"]; !ok || v != nil {
			t.Errorf("%s null cell = %v (present %v); want null", f, v, ok)
		}
	}
}

func TestDocColumnsMetrics(t *testing.T) {
	res := &query.Result{
		View: "table",
		Columns: []query.Column{
			{Path: "status", Type: schema.TInt, Group: true},
			{Path: "count", Type: schema.TInt},
			{Path: "avg(bytes)", Type: schema.TFloat},
			{Path: "p95(bytes)", Type: schema.TInt},
			{Path: "sum(bytes)", Type: schema.TInt},
			{Path: "max(duration_ms)", Type: schema.TDuration},
			{Path: "error_rate", Type: schema.TFloat},
		},
	}
	got := Columns(res, schema.Schema{})
	want := []ColumnDesc{
		{Name: "status", Type: "BIGINT"},
		{Name: "count", Type: "BIGINT"},
		{Name: "avg(bytes)", Type: "DOUBLE"},
		{Name: "p95(bytes)", Type: "DOUBLE"},
		{Name: "sum(bytes)", Type: "BIGINT"},
		{Name: "max(duration_ms)", Type: "DOUBLE", Unit: "ms"},
		{Name: "error_rate", Type: "DOUBLE"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Columns = %+v\nwant %+v", got, want)
	}
}

func TestDocTableNested(t *testing.T) {
	for _, f := range docFormats {
		out := writeWith(t, f, docTable(), statusSchema)
		if f == JSON && !strings.Contains(out, `"status": 200`) {
			t.Errorf("json: status not a number:\n%s", out)
		}
		doc := decode(t, f, out)
		groups := arr(t, doc["groups"])
		if len(groups) != 2 {
			t.Fatalf("%s groups = %v", f, groups)
		}
		get, post := obj(t, groups[0]), obj(t, groups[1])
		if get["method"] != "GET" || post["method"] != "POST" {
			t.Errorf("%s parent order = %v, %v", f, get["method"], post["method"])
		}
		for _, p := range []map[string]any{get, post} {
			if _, ok := p["count"]; ok {
				t.Errorf("%s parent carries count: %v", f, p)
			}
			if len(p) != 2 {
				t.Errorf("%s parent keys = %v; want method, groups", f, p)
			}
		}
		kids := arr(t, get["groups"])
		if len(kids) != 2 {
			t.Fatalf("%s GET groups = %v", f, kids)
		}
		k0, k1 := obj(t, kids[0]), obj(t, kids[1])
		if !isNum(k0["status"]) || numOf(t, k0["status"]) != 200 || numOf(t, k1["status"]) != 500 {
			t.Errorf("%s GET statuses = %v (%T), %v", f, k0["status"], k0["status"], k1["status"])
		}
		if numOf(t, k0["count"]) != 3 || numOf(t, k0["p95(duration_ms)"]) != 23.84 || numOf(t, k0["error_rate"]) != 0 {
			t.Errorf("%s GET 200 = %v", f, k0)
		}
		if v, ok := k1["p95(duration_ms)"]; !ok || v != nil {
			t.Errorf("%s GET 500 p95 = %v; want null", f, v)
		}
		if _, ok := k0["groups"]; ok {
			t.Errorf("%s innermost has groups: %v", f, k0)
		}
	}

	// No group-by: one element with count and the metrics.
	res := &query.Result{
		View:    "table",
		Columns: []query.Column{{Path: "count", Type: schema.TInt}},
		Rows:    []query.Row{{Cells: []query.Cell{{V: int64(9)}}}},
	}
	for _, f := range docFormats {
		groups := arr(t, docOf(t, f, res, schema.Schema{})["groups"])
		if len(groups) != 1 || len(obj(t, groups[0])) != 1 || numOf(t, obj(t, groups[0])["count"]) != 9 {
			t.Errorf("%s no group-by groups = %v", f, groups)
		}
	}
}

func TestDocGraphNested(t *testing.T) {
	for _, f := range docFormats {
		out := writeWith(t, f, docGraph(), statusSchema)
		if strings.Count(out, "2026-11-17T12:01:00.000000005Z") != 1 {
			t.Errorf("%s: starts not written exactly once:\n%s", f, out)
		}
		doc := decode(t, f, out)
		if doc["bucket"] != "1m" || len(arr(t, doc["starts"])) != 3 {
			t.Errorf("%s bucket/starts = %v %v", f, doc["bucket"], doc["starts"])
		}
		series := arr(t, doc["series"])
		if len(series) != 3 {
			t.Fatalf("%s series = %v", f, series)
		}
		get := obj(t, series[0])
		if get["method"] != "GET" || len(get) != 2 {
			t.Errorf("%s first parent = %v", f, get)
		}
		kids := arr(t, get["series"])
		if len(kids) != 2 {
			t.Fatalf("%s GET series = %v", f, kids)
		}
		k0 := obj(t, kids[0])
		if !isNum(k0["status"]) || numOf(t, k0["status"]) != 200 || numOf(t, k0["count"]) != 4 {
			t.Errorf("%s GET 200 = %v", f, k0)
		}
		vals := arr(t, k0["values"])
		if len(vals) != 3 || numOf(t, vals[0]) != 1 || vals[1] != nil || numOf(t, vals[2]) != 3 {
			t.Errorf("%s GET 200 values = %v", f, vals)
		}
		if obj(t, series[1])["method"] != "POST" {
			t.Errorf("%s second parent = %v", f, series[1])
		}
		other := obj(t, series[2])
		if other["other"] != true || len(arr(t, other["values"])) != 3 {
			t.Errorf("%s other = %v", f, other)
		}
		if _, ok := other["method"]; ok {
			t.Errorf("%s other has a group key: %v", f, other)
		}
	}

	// No group-by: one element with only count and values.
	res := &query.Result{View: "graph", Graph: &query.Graph{
		Bucket: "1m", Starts: []time.Time{t0, t0.Add(time.Minute)},
		Metric: query.Column{Path: "p95(duration_ms)", Type: schema.TDuration},
		Series: []query.Series{{Label: "all", Group: []string{}, Count: 2, Values: []*float64{f64(0.25), nil}}},
	}}
	for _, f := range docFormats {
		doc := docOf(t, f, res, schema.Schema{})
		series := arr(t, doc["series"])
		if len(series) != 1 {
			t.Fatalf("%s series = %v", f, series)
		}
		s0 := obj(t, series[0])
		vals := arr(t, s0["values"])
		if len(s0) != 2 || numOf(t, s0["count"]) != 2 || numOf(t, vals[0]) != 250 || vals[1] != nil {
			t.Errorf("%s no group-by series = %v; want {count, values} in ms", f, s0)
		}
		if c := colByName(t, doc, "p95(duration_ms)"); c["type"] != "DOUBLE" || c["unit"] != "ms" || c["nullable"] != true {
			t.Errorf("%s metric column = %v", f, c)
		}
	}
}

func TestDocHeatmapSparse(t *testing.T) {
	for _, f := range docFormats {
		doc := docOf(t, f, docHeatmap(), schema.Schema{})
		bands := arr(t, doc["bands"])
		if len(bands) != 3 {
			t.Fatalf("%s bands = %v", f, bands)
		}
		b0, b1 := obj(t, bands[0]), obj(t, bands[1])
		if _, ok := b0["lo"]; ok || b0["band"] != "<1 ms" || numOf(t, b0["hi"]) != 1 {
			t.Errorf("%s lowest band = %v", f, b0)
		}
		if b1["band"] != "1-10 ms" || numOf(t, b1["lo"]) != 1 || numOf(t, b1["hi"]) != 10 {
			t.Errorf("%s band 1 = %v", f, b1)
		}
		if numOf(t, doc["max"]) != 3 || numOf(t, doc["total"]) != 6 || numOf(t, doc["skipped"]) != 1 {
			t.Errorf("%s max/total/skipped = %v %v %v", f, doc["max"], doc["total"], doc["skipped"])
		}
		buckets := arr(t, doc["buckets"])
		if len(buckets) != 2 {
			t.Fatalf("%s buckets = %v; want the two with spans", f, buckets)
		}
		first, last := obj(t, buckets[0]), obj(t, buckets[1])
		if first["time"] != "2026-11-17T12:00:00.000000005Z" || last["time"] != "2026-11-17T12:02:00.000000005Z" {
			t.Errorf("%s bucket times = %v, %v", f, first["time"], last["time"])
		}
		cells := arr(t, last["cells"])
		if len(cells) != 2 {
			t.Fatalf("%s cells = %v", f, cells)
		}
		c0 := obj(t, cells[0])
		if c0["band"] != "1-10 ms" || numOf(t, c0["count"]) != 2 || len(c0) != 2 {
			t.Errorf("%s cell = %v", f, c0)
		}
	}
}

func TestDocNaN(t *testing.T) {
	res := &query.Result{
		View:    "samples",
		Columns: []query.Column{{Path: "x", Type: schema.TFloat}},
		Rows: []query.Row{
			{Cells: []query.Cell{{V: math.NaN()}}},
			{Cells: []query.Cell{{V: math.Inf(1)}}},
			{Cells: []query.Cell{{V: math.Inf(-1)}}},
		},
	}
	want := []string{"NaN", "Infinity", "-Infinity"}
	for _, f := range []Format{JSON, TOON} {
		rows := arr(t, docOf(t, f, res, schema.Schema{})["rows"])
		for i, w := range want {
			if got := obj(t, rows[i])["x"]; got != w {
				t.Errorf("%s row %d = %v (%T); want %q", f, i, got, got, w)
			}
		}
	}
	out := writeWith(t, YAML, res, schema.Schema{})
	for _, w := range []string{"x: .nan", "x: .inf", "x: -.inf"} {
		if !strings.Contains(out, w) {
			t.Errorf("yaml lacks %q:\n%s", w, out)
		}
	}
	rows := arr(t, decode(t, YAML, out)["rows"])
	if x := numOf(t, obj(t, rows[0])["x"]); !math.IsNaN(x) {
		t.Errorf("yaml NaN decodes as %v", x)
	}
}

func TestDocEmpty(t *testing.T) {
	cases := []struct {
		res *query.Result
		key string
	}{
		{&query.Result{View: "samples"}, "rows"},
		{&query.Result{View: "samples", Columns: []query.Column{{Path: "a"}}, Rows: []query.Row{}}, "rows"},
		{&query.Result{View: "table", Columns: []query.Column{{Path: "count", Type: schema.TInt}}}, "groups"},
		{&query.Result{View: "graph", Graph: &query.Graph{Bucket: "1m", Starts: []time.Time{}, Series: []query.Series{}}}, "series"},
		{&query.Result{View: "graph"}, "series"},
		{&query.Result{View: "heatmap", Heatmap: &query.Heatmap{Bucket: "1m", Starts: []time.Time{t0}, Bands: []query.Band{{Lo: 0, Hi: 1}}, Counts: [][]int{{0}}}}, "buckets"},
		{&query.Result{View: "heatmap"}, "buckets"},
	}
	for i, c := range cases {
		for _, f := range docFormats {
			out := writeWith(t, f, c.res, schema.Schema{})
			if f == JSON && !strings.Contains(out, `"`+c.key+`": []`) {
				t.Errorf("case %d json: no empty %s:\n%s", i, c.key, out)
			}
			if f == YAML && !strings.Contains(out, c.key+": []") {
				t.Errorf("case %d yaml: no empty %s:\n%s", i, c.key, out)
			}
			doc := decode(t, f, out)
			v, ok := doc[c.key]
			if !ok {
				t.Errorf("case %d %s: %s missing", i, f, c.key)
				continue
			}
			if a, isArr := v.([]any); !isArr || len(a) != 0 {
				t.Errorf("case %d %s: %s = %#v; want []", i, f, c.key, v)
			}
			if a, isArr := doc["columns"].([]any); !isArr || a == nil {
				t.Errorf("case %d %s: columns = %#v", i, f, doc["columns"])
			}
		}
	}
}

func TestDocJSONField(t *testing.T) {
	res := &query.Result{
		View:    "samples",
		Columns: []query.Column{{Path: "j", Type: schema.TJSON}},
		Rows:    []query.Row{{Cells: []query.Cell{{V: `{"z": 1, "a": [true, "x|y"], "m": {"k": null}}`}}}},
	}
	for _, f := range docFormats {
		out := writeWith(t, f, res, schema.Schema{})
		if zi, ai := strings.Index(out, "z"), strings.Index(out, `a`+map[Format]string{JSON: `"`, YAML: ":", TOON: "["}[f]); zi < 0 || ai < 0 || zi > ai {
			t.Errorf("%s: JSON field keys out of order:\n%s", f, out)
		}
		j := obj(t, obj(t, arr(t, decode(t, f, out)["rows"])[0])["j"])
		if numOf(t, j["z"]) != 1 {
			t.Errorf("%s z = %v", f, j["z"])
		}
		a := arr(t, j["a"])
		if len(a) != 2 || a[0] != true || a[1] != "x|y" {
			t.Errorf("%s a = %v", f, a)
		}
		if m := obj(t, j["m"]); len(m) != 1 || m["k"] != nil {
			t.Errorf("%s m = %v", f, m)
		}
		if c := colByName(t, decode(t, f, out), "j"); c["type"] != "JSON" {
			t.Errorf("%s j column = %v", f, c)
		}
	}
}

func TestDocStringsStayStrings(t *testing.T) {
	// A string that reads as another type in YAML or TOON stays a string.
	res := &query.Result{
		View:    "samples",
		Columns: []query.Column{{Path: "s", Type: schema.TString}},
		Rows: []query.Row{
			{Cells: []query.Cell{{V: "200"}}}, {Cells: []query.Cell{{V: "true"}}}, {Cells: []query.Cell{{V: "null"}}},
			{Cells: []query.Cell{{V: tricky}}}, {Cells: []query.Cell{{V: "- a: b"}}}, {Cells: []query.Cell{{V: ""}}},
		},
	}
	want := []string{"200", "true", "null", tricky, "- a: b", ""}
	for _, f := range docFormats {
		rows := arr(t, docOf(t, f, res, schema.Schema{})["rows"])
		for i, w := range want {
			if got := obj(t, rows[i])["s"]; got != w {
				t.Errorf("%s row %d = %#v; want %q", f, i, got, w)
			}
		}
	}
}

func TestFlatDurationMs(t *testing.T) {
	res := &query.Result{
		View:    "samples",
		Columns: []query.Column{{Path: "d", Type: schema.TDuration}},
		Rows:    []query.Row{{Cells: []query.Cell{{V: 0.087}}}, {Cells: []query.Cell{{V: 0.3}}}},
	}
	if got := write(t, CSV, res); got != "d\n87\n300\n" {
		t.Errorf("csv = %q; want durations in ms", got)
	}
	g := &query.Result{View: "graph", Graph: &query.Graph{
		Starts: []time.Time{t0},
		Metric: query.Column{Path: "avg(d)", Type: schema.TDuration},
		Series: []query.Series{{Label: "all", Group: []string{}, Values: []*float64{f64(0.0123)}}},
	}}
	if got := write(t, CSV, g); got != "time,avg(d)\n2026-11-17T12:00:00.000000005Z,12.3\n" {
		t.Errorf("graph csv = %q; want ms", got)
	}
}
