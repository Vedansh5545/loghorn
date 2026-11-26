package duckstore

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/schema"
)

var modelRunStart = time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)

func appTable() ModelTable {
	return ModelTable{Source: record.SourceLog, Stream: "app", Fields: []schema.Field{
		{Path: "code", Type: schema.TInt},
		{Path: "latency", Type: schema.TDuration},
		{Path: "Code", Type: schema.TInt},       // same column as code in DuckDB: no column
		{Path: "Service", Type: schema.TString}, // a model column: no column
		{Path: "when", Type: schema.TTimestamp},
		{Path: "http.method", Type: schema.TString},
	}}
}

func spanTable() ModelTable {
	return ModelTable{Source: record.SourceOtel, Stream: record.AllStreams, Fields: []schema.Field{
		{Path: "duration_ms", Type: schema.TFloat},
		{Path: "name", Type: schema.TString},
	}}
}

// withModel applies the model of tables and runs fn under it.
func withModel(t *testing.T, g *rig, runID string, fn func(), tables ...ModelTable) {
	t.Helper()
	err := g.e.WithModel(context.Background(), ModelDDL(tables, modelRunStart, runID), func() error { fn(); return nil })
	if err != nil {
		t.Fatal(err)
	}
}

func TestModelViews(t *testing.T) {
	g := newRig(t)
	withModel(t, g, "", func() {
		rows, err := g.e.DB().Query(`SELECT column_name, data_type FROM information_schema.columns
			WHERE table_catalog = 'memory' AND table_schema = 'logs' AND table_name = 'app' ORDER BY ordinal_position`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var n, ty string
			rows.Scan(&n, &ty)
			got = append(got, n+" "+ty)
		}
		want := "seq BIGINT|time TIMESTAMP WITH TIME ZONE|service VARCHAR|raw VARCHAR|code BIGINT|latency DOUBLE|when TIMESTAMP WITH TIME ZONE|http.method VARCHAR"
		if strings.Join(got, "|") != want {
			t.Fatalf("logs.app columns\n got %s\nwant %s", strings.Join(got, "|"), want)
		}
		if n := g.one(`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'spans' AND table_name = '*'`); n != int64(1) {
			t.Fatalf("spans.\"*\" missing: %v", n)
		}
	}, appTable(), spanTable())
}

func TestModelValues(t *testing.T) {
	g := newRig(t)
	g.add(
		logRec(t, "api", "app", `,"code":200,"latency":"1.1s","when":"2026-11-23T10:00:01Z","http":{"method":"GET"}`),
		logRec(t, "web", "app", `,"code":"oops","latency":"12ms"`),
		spanRec(t, `{"trace_id":"0af7651916cd43dd8448eb211c80319c","span_id":"a000000000000001","name":"chat","kind":"client","start":"2026-11-17T17:01:01.000Z","end":"2026-11-17T17:01:02.500Z","duration_ms":1500,"service.name":"svc"}`),
	)
	withModel(t, g, "", func() {
		rows, err := g.e.DB().Query(`SELECT service, code, latency, "when" = TIMESTAMPTZ '2026-11-23 10:00:01+00', "http.method", raw LIKE '%oops%' FROM logs."app" ORDER BY seq`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var svc string
			var code, lat, when, method any
			var oops bool
			if err := rows.Scan(&svc, &code, &lat, &when, &method, &oops); err != nil {
				t.Fatal(err)
			}
			got = append(got, strings.Join([]string{svc, str(code), str(lat), str(when), str(method), str(oops)}, ","))
		}
		want := []string{"api,200,1100,true,GET,false", "web,<nil>,12,<nil>,<nil>,true"}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("got  %v\nwant %v", got, want)
		}
		if v := g.one(`SELECT name || ' ' || duration_ms FROM spans."*"`); v != "chat 1500.0" {
			t.Fatalf("spans.\"*\" = %v", v)
		}
	}, appTable(), spanTable())
}

func TestModelMacros(t *testing.T) {
	g := newRig(t)
	withModel(t, g, "run-7", func() {
		if v := g.one(`SELECT loghorn.this_run_id()`); v != "run-7" {
			t.Fatalf("this_run_id = %v", v)
		}
		if v := g.one(`SELECT loghorn.this_run_start() = TIMESTAMPTZ '2026-11-23 09:00:00+00'`); v != true {
			t.Fatalf("this_run_start = %v", v)
		}
	}, appTable())
	withModel(t, g, "", func() {
		if v := g.one(`SELECT loghorn.this_run_id() IS NULL`); v != true {
			t.Fatalf("this_run_id with no run = %v", v)
		}
	}, appTable())
}

func TestModelOddStreamName(t *testing.T) {
	g := newRig(t)
	g.add(logRec(t, "api", `a"b.c`, ``))
	withModel(t, g, "", func() {
		if v := g.one(`SELECT count(*) FROM logs."a""b.c"`); v != int64(1) {
			t.Fatalf("count = %v", v)
		}
	}, ModelTable{Source: record.SourceLog, Stream: `a"b.c`})
}

func TestModelDDLAppliedOnlyWhenChanged(t *testing.T) {
	g := newRig(t)
	withModel(t, g, "", func() {}, appTable())
	if _, err := g.e.DB().Exec(`DROP VIEW memory.logs."app"`); err != nil {
		t.Fatal(err)
	}
	withModel(t, g, "", func() {
		if v := g.one(`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'logs'`); v != int64(0) {
			t.Fatalf("same DDL ran again: %v views", v)
		}
	}, appTable())
	withModel(t, g, "", func() {
		if v := g.one(`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'logs'`); v != int64(1) {
			t.Fatalf("changed DDL: %v views, want only web", v)
		}
		if v := g.one(`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'logs' AND table_name = 'web'`); v != int64(1) {
			t.Fatal("logs.web missing")
		}
	}, ModelTable{Source: record.SourceLog, Stream: "web"})
}

func TestLatestRun(t *testing.T) {
	g := newRig(t)
	if _, _, ok := g.e.LatestRun(context.Background()); ok {
		t.Fatal("a run in an empty store")
	}
	a := logRec(t, "api", "app", `,"loghorn.run.id":"r1"`)
	b := logRec(t, "api", "app", `,"loghorn.run.id":"r2"`)
	c := logRec(t, "api", "app", `,"loghorn.run.id":"r2"`)
	d := logRec(t, "api", "app", ``)
	a.Time = modelRunStart
	b.Time = modelRunStart.Add(time.Minute)
	c.Time = modelRunStart.Add(2 * time.Minute)
	d.Time = modelRunStart.Add(3 * time.Minute)
	g.add(a, b, c, d)
	id, start, ok := g.e.LatestRun(context.Background())
	if !ok || id != "r2" || !start.Equal(b.Time) {
		t.Fatalf("LatestRun = %q %v %v", id, start, ok)
	}
}

func str(v any) string { return fmt.Sprint(v) }

func TestModelTablesView(t *testing.T) {
	g := newRig(t)
	withModel(t, g, "", func() {
		rows, err := g.e.DB().Query(`SELECT name, columns FROM loghorn.tables`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var n, c string
			rows.Scan(&n, &c)
			got = append(got, n+" | "+c)
		}
		want := []string{
			`logs."app" | seq BIGINT, "time" TIMESTAMPTZ, service VARCHAR, raw VARCHAR, code BIGINT, latency DOUBLE, "when" TIMESTAMPTZ, "http.method" VARCHAR`,
			`spans."*" | seq BIGINT, "time" TIMESTAMPTZ, service VARCHAR, raw VARCHAR, duration_ms DOUBLE, name VARCHAR`,
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}, appTable(), spanTable())
}

// TestModelNameClashes: streams whose names differ only in case, or a
// stream named "*", must not break the model; for fields that differ only in
// case, the one with the most values gets the column.
func TestModelNameClashes(t *testing.T) {
	g := newRig(t)
	g.add(logRec(t, "api", "app", `,"code":200`), logRec(t, "api", "app", `,"code":300`), logRec(t, "api", "app", `,"Code":"x"`))
	withModel(t, g, "", func() {
		rows, err := g.e.DB().Query(`SELECT code FROM logs."app" ORDER BY seq`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var v any
			rows.Scan(&v)
			got = append(got, fmt.Sprint(v))
		}
		if strings.Join(got, ",") != "200,300,<nil>" {
			t.Fatalf("code = %v", got)
		}
		if n := g.one(`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'logs'`); n != int64(2) {
			t.Fatalf("%v log views, want app and *", n)
		}
	},
		ModelTable{Source: record.SourceLog, Stream: "app", Fields: []schema.Field{
			{Path: "Code", Type: schema.TString, Count: 1}, {Path: "code", Type: schema.TInt, Count: 2}}},
		ModelTable{Source: record.SourceLog, Stream: "App"},
		ModelTable{Source: record.SourceLog, Stream: record.AllStreams},
		ModelTable{Source: record.SourceLog, Stream: record.AllStreams},
	)
}
