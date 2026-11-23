package query_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/adapter"
	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/duckstore"
	"github.com/jimbeveridge/loghorn/internal/ingest"
	"github.com/jimbeveridge/loghorn/internal/query"
	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/schema"
)

// parityLines is the generated data set: two services, two log streams,
// mixed and clashing types, untimed records, case-clashing and dotted
// names, and spans with parents, events and every duration shape. Times
// cross the 2026-03-08 US daylight-saving change.
func parityLines() []string {
	base := time.Date(2026, 3, 7, 20, 0, 0, 0, time.UTC)
	var out []string
	sevs := []string{"INFO", "WARNING", "ERROR", "error ", "DEBUG", "CRITICAL"}
	durs := []string{`"12ms"`, `"1.5s"`, `"250us"`, `"2m"`}
	for i := 0; i < 160; i++ {
		svc := []string{"alpha", "beta"}[i%2]
		stream := "api"
		if i%3 == 0 {
			stream = "worker"
		}
		var b strings.Builder
		fmt.Fprintf(&b, `{"severity":%q,"message":"msg %d %s","labels":{"schema":%q},"service":{"name":%q}`, sevs[i%len(sevs)], i%9, []string{"Alpha", "ünïcode", "Error here"}[i%3], stream, svc)
		if i%17 != 5 && i != 0 {
			fmt.Fprintf(&b, `,"timestamp":%q`, base.Add(time.Duration(i)*23*time.Minute+time.Duration(i%7)*time.Millisecond).Format(time.RFC3339Nano))
		}
		if i%11 != 0 {
			fmt.Fprintf(&b, `,"n":%d`, i%7)
		}
		if svc == "alpha" {
			fmt.Fprintf(&b, `,"f":%v,"mixed":%d`, float64(i)*0.25+0.5, i%5)
		} else {
			fmt.Fprintf(&b, `,"f":%d,"mixed":"m%d"`, i%4, i%3)
		}
		fmt.Fprintf(&b, `,"b":%v,"d":%s,"latency":%d`, i%2 == 0, durs[i%len(durs)], i*3%50)
		if i%4 == 0 {
			fmt.Fprintf(&b, `,"tags":["t%d","x"]`, i%3)
		}
		if i%5 == 0 {
			fmt.Fprintf(&b, `,"userId":%d`, i)
		} else if i%5 == 1 {
			fmt.Fprintf(&b, `,"userID":"u%d"`, i)
		}
		if i%6 == 0 {
			fmt.Fprintf(&b, `,"a.b":"dot%d","empty":"","nul":null`, i%2)
		}
		if i%13 == 0 {
			b.WriteString(`,"receiveTimestamp":"not a time"`)
		} else if i%13 == 1 {
			fmt.Fprintf(&b, `,"receiveTimestamp":%q`, base.Add(time.Duration(i)*time.Hour).Format(time.RFC3339))
		}
		if i%10 == 3 {
			fmt.Fprintf(&b, `,"trace":"projects/p/traces/%032x","spanId":"%016x"`, i%4+1, i)
		}
		b.WriteString("}")
		out = append(out, b.String())
	}
	durations := []string{`0.5`, `12`, `0`, `-1`, `4800.25`, `33`, `1`, `100`, ``, `"9ms"`}
	for i := 0; i < 80; i++ {
		trace := i%10 + 1
		start := base.Add(time.Duration(i) * 41 * time.Minute)
		name, kind, attrs := "GET /a", "server", `{"http.request.method":"GET","http.route":"/a"}`
		switch i % 4 {
		case 1:
			name, kind, attrs = "db query", "client", fmt.Sprintf(`{"db.system":"postgresql","db.statement":"select %d"}`, i%3)
		case 2:
			name, kind, attrs = "work item", "internal", `{"job":"x"}`
		case 3:
			name, kind, attrs = "GET /a", "internal", `{}`
		}
		parent := ""
		if i%4 != 0 {
			parent = fmt.Sprintf(`,"parent_span_id":"%016x"`, (i/4)*4+1)
		}
		dur := ""
		if d := durations[i%len(durations)]; d != "" {
			dur = `,"duration_ms":` + d
		}
		svc := []string{"shop", "pay"}[i%3%2]
		events := ""
		if i%7 == 0 {
			events = `,"events":[{"name":"retry"}]`
		}
		out = append(out, fmt.Sprintf(`{"trace_id":"%032x","span_id":"%016x","name":%q,"kind":%q,"start":%q,"end":%q%s%s,"status":%q,"attributes":%s,"service.name":%q%s}`,
			trace, i+1, name, kind, start.Format(time.RFC3339Nano), start.Add(time.Second).Format(time.RFC3339Nano), parent, dur,
			[]string{"ok", "error"}[i%5/4], attrs, svc, events))
	}
	return out
}

// parityRecords normalizes the fixtures and the generated lines and
// numbers them the way the store does: Seq from 1, the default service,
// and inherited times (an untimed leading record takes the next time).
func parityRecords(t *testing.T) []record.Record {
	t.Helper()
	in := config.Input{}
	p := adapter.New(in)
	var raw [][]byte
	paths, _ := filepath.Glob("../../testdata/web/*")
	sort.Strings(paths)
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		ingest.Stored(f, func(l []byte) { raw = append(raw, bytes.Clone(l)) })
		f.Close()
	}
	for _, l := range parityLines() {
		raw = append(raw, []byte(l))
	}
	var recs []record.Record
	var pending []int
	for _, l := range raw {
		r, ok := record.Normalize(p.ParseLine(l), in)
		if !ok {
			continue
		}
		r.Seq = len(recs) + 1
		if r.Service == "" {
			r.Service = record.DefaultService
		}
		if r.Time.IsZero() {
			r.TimeInherited = true
			if len(recs) > 0 {
				r.Time = recs[len(recs)-1].Time
			}
		}
		if r.Time.IsZero() {
			pending = append(pending, len(recs))
		} else {
			for _, i := range pending {
				recs[i].Time = r.Time
			}
			pending = nil
		}
		recs = append(recs, r)
	}
	return recs
}

type backend struct{ *duckstore.Engine }

func newBackend(t *testing.T, recs []record.Record) backend {
	t.Helper()
	e, err := duckstore.OpenMemory(config.Input{}, func(s string) { t.Log(s) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	for _, r := range recs {
		e.Add(r)
	}
	if !e.Sync(30 * time.Second) {
		t.Fatal("sync timed out")
	}
	return backend{e}
}

func (b backend) schema(t *testing.T, svc, src, stream string) schema.Schema {
	t.Helper()
	tl, err := b.Tallies(src, svc, stream)
	if err != nil {
		t.Fatal(err)
	}
	return schema.FromTallies(src, stream, tl)
}

type streamCase struct{ src, stream, svc string }

func parityStreams() []streamCase {
	var out []streamCase
	for _, svc := range []string{"*", "alpha", "shop"} {
		for _, s := range []streamCase{
			{"log", "api", svc}, {"log", "worker", svc}, {"log", "*", svc},
			{"otel", "*", svc}, {"otel", "db", svc}, {"otel", "http.server", svc}, {"otel", "GET /a", svc}, {"otel", "work item", svc},
		} {
			out = append(out, s)
		}
	}
	return out
}

// operands picks filter values for a field from the data.
func operands(f schema.Field, recs []record.Record) []string {
	seen := map[string]bool{}
	var vals []string
	for _, r := range recs {
		v := schema.Convert(r.Fields[f.Path], f)
		if v.Bad || v.Empty {
			continue
		}
		var s string
		switch f.Type {
		case schema.TInt:
			s = fmt.Sprint(v.Int)
		case schema.TFloat:
			s = fmt.Sprint(v.Num)
		case schema.TDuration:
			s = fmt.Sprintf("%gs", v.Num)
		case schema.TBool:
			s = fmt.Sprint(v.Bool)
		case schema.TTimestamp:
			s = v.Time.Format(time.RFC3339Nano)
		case schema.TJSON:
			s = "t1"
		default:
			s = v.Str
			if len(s) > 4 {
				s = strings.ToUpper(s[:3])
			}
		}
		if !seen[s] {
			seen[s] = true
			vals = append(vals, s)
		}
		if len(vals) == 2 {
			break
		}
	}
	if len(vals) == 0 {
		vals = []string{"0"}
	}
	return vals
}

func parityQueries(sch schema.Schema, sc streamCase, recs []record.Record) []query.Query {
	base := query.Query{Stream: sc.stream, Source: sc.src, Service: sc.svc}
	var qs []query.Query
	add := func(f func(q *query.Query)) {
		q := base
		f(&q)
		qs = append(qs, q)
	}
	add(func(q *query.Query) {})
	add(func(q *query.Query) { q.Limit, q.Offset = 5, 3 })
	add(func(q *query.Query) { q.Search = "error" })
	add(func(q *query.Query) { q.Search = "ÜNÏ" })
	add(func(q *query.Query) {
		q.Range = query.Range{From: "2026-03-08T01:00:00Z", To: "2026-03-08T20:00:00.000000001Z"}
	})
	add(func(q *query.Query) { q.Range = query.Range{To: "2026-03-08T01:00:00Z"} })
	add(func(q *query.Query) { q.Columns = []string{"n", "missing", "tags"} })
	var numeric []schema.Field
	for _, f := range sch.Fields {
		f := f
		add(func(q *query.Query) { q.Sort = query.Sort{Field: f.Path} })
		add(func(q *query.Query) { q.Sort = query.Sort{Field: f.Path, Desc: true}; q.Limit = 7 })
		for _, op := range query.OpsFor(f.Type) {
			vals := operands(f, recs)
			if op != "in" && op != "not-in" {
				vals = vals[:1]
			}
			if op == "empty" || op == "not-empty" {
				vals = nil
			}
			add(func(q *query.Query) { q.Filters = []query.Filter{{Field: f.Path, Op: op, Values: vals}} })
		}
		add(func(q *query.Query) { q.View = "table"; q.GroupBy = []string{f.Path} })
		if f.Type == schema.TInt || f.Type == schema.TFloat || f.Type == schema.TDuration {
			numeric = append(numeric, f)
		}
	}
	for _, f := range numeric {
		var aggs []query.Aggregate
		for _, op := range query.AggregateOps {
			a := query.Aggregate{Op: op, Field: f.Path}
			if op == "error_rate" {
				a.Field = ""
			}
			aggs = append(aggs, a)
		}
		for _, gb := range [][]string{nil, {"severity"}, {"b", "n"}} {
			for _, o := range []*query.Order{nil, {Op: "p90", Field: f.Path}, {Op: "avg", Field: f.Path, Asc: true}, {Op: "error_rate"}} {
				add(func(q *query.Query) {
					q.View, q.GroupBy, q.Aggregates, q.Order, q.Top = "table", gb, aggs, o, 2
				})
			}
		}
		for _, bucket := range []string{"auto", "5m", "1h", "6h", "1d"} {
			for _, plot := range []*query.Aggregate{nil, {Op: "p50", Field: f.Path}, {Op: "sum", Field: f.Path}} {
				add(func(q *query.Query) {
					q.View, q.Bucket, q.GroupBy, q.Aggregates, q.Plot, q.Top = "graph", bucket, []string{"severity"}, aggs, plot, 1
				})
			}
		}
		add(func(q *query.Query) { q.View, q.Aggregates, q.Plot = "graph", aggs, &query.Aggregate{Op: "max", Field: f.Path} })
		break // one numeric field is enough for the aggregate grid
	}
	add(func(q *query.Query) { q.View = "graph"; q.Bucket = "1s" })
	add(func(q *query.Query) { q.View = "graph"; q.GroupBy = []string{"tags"}; q.Order = &query.Order{Op: "count", Asc: true} })
	if sc.src == "otel" {
		for _, bucket := range []string{"auto", "15m", "6h"} {
			add(func(q *query.Query) { q.View = "heatmap"; q.Bucket = bucket })
		}
		lo := 1.0
		add(func(q *query.Query) {
			q.Cell = &query.HeatCell{From: time.Date(2026, 3, 7, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC), Lo: &lo, Hi: 100}
		})
		add(func(q *query.Query) {
			q.View = "heatmap"
			q.Cell = &query.HeatCell{From: time.Date(2026, 3, 7, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC), Hi: 50}
		})
	}
	return qs
}

func TestRunSQLMatchesRun(t *testing.T) {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		old := time.Local
		time.Local = loc
		t.Cleanup(func() { time.Local = old })
	} else {
		t.Logf("no tzdata, DST not covered: %v", err)
	}
	recs := parityRecords(t)
	b := newBackend(t, recs)
	now := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	overrides := [][]schema.Field{nil,
		{{Path: "n", Type: schema.TFloat}, {Path: "mixed", Type: schema.TInt}, {Path: "b", Type: schema.TString}, {Path: "latency", Type: schema.TDuration}},
		{{Path: "f", Type: schema.TString}, {Path: "message", Type: schema.TJSON}, {Path: "duration_ms", Type: schema.TInt}},
	}
	ran := 0
	start := time.Now()
	defer func() { t.Logf("%d queries compared in %v", ran, time.Since(start)) }()
	for _, sc := range parityStreams() {
		if f := os.Getenv("PARITY_ONLY"); f != "" && !strings.Contains(fmt.Sprint(sc), f) {
			continue
		}
		want := schema.Infer(sc.svc, sc.src, sc.stream, recs)
		got := b.schema(t, sc.svc, sc.src, sc.stream)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%v schema:\n got %+v\nwant %+v", sc, got, want)
			continue
		}
		for oi, saved := range overrides {
			if oi > 0 && sc.svc != "*" {
				continue // overrides once per stream
			}
			sch := schema.Effective(want, saved)
			for qi, q := range parityQueries(sch, sc, recs) {
				if os.Getenv("PARITY_FULL") == "" && qi%10 != 0 {
					continue // PARITY_FULL=1 compares every query (several minutes)
				}
				if knownLossy(recs, sc, sch, q) {
					continue
				}
				ran++
				wres, werr := query.Run(recs, sch, q, now, time.Time{})
				gres, gerr := query.RunSQL(context.Background(), b, sch, q, now, time.Time{})
				name := fmt.Sprintf("%v override %d query %s", sc, oi, mustJSON(q))
				if (werr != nil) != (gerr != nil) {
					t.Errorf("%s: error %v, want %v", name, gerr, werr)
					continue
				}
				if werr != nil {
					continue
				}
				if diff := compareJSON(mustAny(gres), mustAny(wres), "$"); diff != "" {
					t.Errorf("%s:\n%s", name, diff)
				}
			}
		}
	}
}

// knownLossy skips queries that filter, group or sort on a field read as a
// type whose stored column, in some service, lacks the text as received:
// string or JSON over float, timestamp or duration, and int over float
// (D-011 #1, which also covers types that differ between services).
func knownLossy(recs []record.Record, sc streamCase, sch schema.Schema, q query.Query) bool {
	// stored is each table's column type for path: per service and table
	// (stream for logs, span type for spans), as the store types it.
	stored := func(path string) []schema.Type {
		kinds := map[[2]string]schema.KindSet{}
		for _, r := range recs {
			k := r.Key()
			if k.Source != sc.src || (sc.svc != "*" && r.Service != sc.svc) {
				continue
			}
			if sc.stream != record.AllStreams && k.Name != sc.stream {
				continue
			}
			v, ok := r.Fields[path]
			if !ok {
				continue
			}
			tk := r.Stream
			if k.Source == record.SourceOtel {
				tk, _ = r.Fields["span.type"].(string)
			}
			kinds[[2]string{r.Service, tk}] |= schema.Classify(v)
		}
		var out []schema.Type
		for _, k := range kinds {
			out = append(out, schema.TallyType(path, k))
		}
		return out
	}
	lossy := func(path string) bool {
		f, ok := sch.Field(path)
		if !ok {
			return false
		}
		for _, from := range stored(path) {
			if from == f.Type {
				continue
			}
			if (f.Type == schema.TString || f.Type == schema.TJSON) && (from == schema.TFloat || from == schema.TTimestamp || from == schema.TDuration) {
				return true
			}
			if from == schema.TFloat && f.Type == schema.TInt {
				return true
			}
		}
		return false
	}
	for _, f := range q.Filters {
		if lossy(f.Field) {
			return true
		}
	}
	for _, p := range q.GroupBy {
		if lossy(p) {
			return true
		}
	}
	return lossy(q.Sort.Field)
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func mustAny(v any) any {
	var out any
	d := json.NewDecoder(strings.NewReader(mustJSON(v)))
	d.UseNumber()
	if err := d.Decode(&out); err != nil {
		panic(err)
	}
	return out
}

// compareJSON compares decoded JSON, allowing a relative difference of 1e-9
// between non-integer numbers (sum and avg add in a different order).
func compareJSON(got, want any, path string) string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s: got %v, want object", path, got)
		}
		keys := map[string]bool{}
		for k := range w {
			keys[k] = true
		}
		for k := range g {
			keys[k] = true
		}
		var out []string
		for k := range keys {
			if d := compareJSON(g[k], w[k], path+"."+k); d != "" {
				out = append(out, d)
			}
		}
		sort.Strings(out)
		return strings.Join(out, "\n")
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return fmt.Sprintf("%s: got %s, want %s", path, short(got), short(want))
		}
		for i := range w {
			if d := compareJSON(g[i], w[i], fmt.Sprintf("%s[%d]", path, i)); d != "" {
				return d
			}
		}
		return ""
	case json.Number:
		g, ok := got.(json.Number)
		if !ok {
			return fmt.Sprintf("%s: got %v, want %v", path, got, w)
		}
		if g == w {
			return ""
		}
		x, _ := g.Float64()
		y, _ := w.Float64()
		if math.Abs(x-y) <= 1e-9*math.Max(math.Abs(x), math.Abs(y)) {
			return ""
		}
		return fmt.Sprintf("%s: got %v, want %v", path, g, w)
	}
	if !reflect.DeepEqual(got, want) {
		return fmt.Sprintf("%s: got %s, want %s", path, short(got), short(want))
	}
	return ""
}

func short(v any) string {
	s := mustJSON(v)
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return s
}

func BenchmarkRunSQLSmall(b *testing.B) {
	t := &testing.T{}
	recs := parityRecords(t)
	e, err := duckstore.OpenMemory(config.Input{}, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer e.Close()
	for _, r := range recs {
		e.Add(r)
	}
	e.Sync(time.Minute)
	be := backend{e}
	tl, _ := e.Tallies("log", "*", "api")
	sch := schema.FromTallies("log", "api", tl)
	now := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	for _, q := range []query.Query{
		{Stream: "api", Source: "log"},
		{Stream: "api", Source: "log", View: "table", GroupBy: []string{"severity"}},
	} {
		b.Run(q.View, func(b *testing.B) {
			for b.Loop() {
				if _, err := query.RunSQL(context.Background(), be, sch, q, now, time.Time{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	b.Run("count", func(b *testing.B) {
		for b.Loop() {
			var n int
			e.DB().QueryRow("SELECT count(*) FROM all_records").Scan(&n)
		}
	})
}
