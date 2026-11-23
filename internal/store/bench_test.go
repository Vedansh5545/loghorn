package store

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/adapter"
	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/query"
	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/schema"
)

// TestSpeedAtScale times the web view's requests over a spike-2 sized day
// (about 770k records: 8 log streams in 3 services, and spans of 3 types),
// stored in day files, against the ~50 ms target (notes #33). It is slow, so
// it runs only with LOGHORN_SPEED=1, and reports rather than fails.
func TestSpeedAtScale(t *testing.T) {
	if os.Getenv("LOGHORN_SPEED") == "" {
		t.Skip("set LOGHORN_SPEED=1 to time requests at scale")
	}
	n := 770_000
	dir := t.TempDir()
	start := time.Date(2026, 11, 23, 0, 0, 0, 0, time.Local)
	s, err := Open(Options{Mode: ModeLive, Dir: dir, Write: true, Input: config.Input{}, Now: func() time.Time { return start.Add(20 * time.Hour) }, Warn: func(m string) { t.Log(m) }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sevs := []string{"INFO", "INFO", "INFO", "WARNING", "ERROR", "DEBUG"}
	routes := []string{"/api/users", "/api/orders", "/api/cart", "/health", "/api/search"}
	t0 := time.Now()
	for i := 0; i < n; i++ {
		ts := start.Add(time.Duration(i) * 90 * time.Millisecond)
		var line string
		if i%5 == 4 { // spans: 20%
			typ := []string{`"kind":"client","attributes":{"db.system":"postgresql","db.statement":"select 1"}`, `"kind":"server","attributes":{"http.request.method":"GET","http.route":"/x"}`, `"kind":"internal","attributes":{"job":"j"}`}[i%3]
			line = fmt.Sprintf(`{"trace_id":"%032x","span_id":"%016x","parent_span_id":"%016x","name":"op%d","start":%q,"end":%q,"duration_ms":%v,"status":"ok",%s,"service.name":"svc%d"}`,
				i/20+1, i+1, (i/20)*20+1, i%7, ts.UTC().Format(time.RFC3339Nano), ts.Add(time.Millisecond).UTC().Format(time.RFC3339Nano), float64(i%997)*0.37+0.1, typ, i%3)
		} else {
			line = fmt.Sprintf(`{"timestamp":%q,"severity":%q,"message":"request %d handled for user %d","labels":{"schema":"stream%d"},"service":{"name":"svc%d"},"httpRequest":{"requestMethod":"GET","requestUrl":%q,"status":%d,"latency":"%0.3fs"},"userId":%d,"bytes":%d,"region":"r%d","cache":%v,"retries":%d,"tags":["a","b"]}`,
				ts.UTC().Format(time.RFC3339Nano), sevs[i%len(sevs)], i, i%5000, i%8, i%3, routes[i%len(routes)], []int{200, 200, 201, 404, 500}[i%5], float64(i%1000)/997, i%5000, i*13%65536, i%4, i%2 == 0, i%3)
		}
		var r record.Record
		var ok bool
		if strings.HasPrefix(line, `{"trace_id"`) {
			r, ok = record.NormalizeSpan([]byte(line))
		} else {
			r, ok = logRecordOK(line)
		}
		if !ok {
			t.Fatalf("bad line %s", line)
		}
		s.Add(r, ts)
		if i%50_000 == 0 {
			s.Flush() // a burst past MaxQueue would be skipped, by design
		}
	}
	s.Flush()
	t.Logf("wrote %d records in %v", n, time.Since(t0))
	all := s.Records()
	if len(all) != n {
		t.Fatalf("stored %d, want %d", len(all), n)
	}
	now := start.Add(24 * time.Hour)

	measure := func(name string, f func() error) {
		var ds []time.Duration
		for i := 0; i < 5; i++ {
			st := time.Now()
			if err := f(); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			ds = append(ds, time.Since(st))
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		t.Logf("%-40s median %v", name, ds[len(ds)/2].Round(100*time.Microsecond))
	}
	run := func(name string, q query.Query) {
		src, _ := record.ParseSource(q.Source)
		sch, err := s.Schema(q.Service, src, q.Stream)
		if err != nil {
			t.Fatal(err)
		}
		sch = schema.Effective(sch, nil)
		measure("duckdb "+name, func() error { _, err := s.Query(context.Background(), sch, q, now); return err })
		measure("query.Run "+name, func() error { _, err := query.Run(all, sch, q, now, time.Time{}); return err })
	}
	run("samples stream", query.Query{Stream: "stream1", Service: "*"})
	run("samples filter+sort", query.Query{Stream: "stream1", Service: "*", Filters: []query.Filter{{Field: "httpRequest.status", Op: ">=", Values: []string{"400"}}}, Sort: query.Sort{Field: "bytes", Desc: true}})
	run("search", query.Query{Stream: "stream1", Service: "*", Search: "user 4242"})
	run("table p95 by status", query.Query{Stream: "stream1", Service: "*", View: "table", GroupBy: []string{"httpRequest.status"}, Aggregates: []query.Aggregate{{Op: "p95", Field: "httpRequest.latency"}, {Op: "error_rate"}}})
	run("graph auto by severity", query.Query{Stream: "stream1", Service: "*", View: "graph", GroupBy: []string{"severity"}})
	run("graph all kinds", query.Query{Stream: "*", Service: "*", View: "graph"})
	run("heatmap db spans", query.Query{Stream: "db", Source: "otel", Service: "*", View: "heatmap"})
	measure("kinds", func() error { s.Kinds("log", "*", time.Time{}, time.Time{}); return nil })
	measure("schema (cache cold)", func() error { s.cache = map[[3]string]cached{}; _, err := s.Schema("*", "log", "stream1"); return err })
	measure("trace", func() error { s.TraceRecords(fmt.Sprintf("%032x", 1000)); return nil })
	if fi, err := os.Stat(dir + "/logs-2026-11-23.duckdb"); err == nil {
		t.Logf("logs file %d MB", fi.Size()>>20)
	}
}

func logRecordOK(line string) (record.Record, bool) {
	return record.Normalize(parser.ParseLine([]byte(line)), config.Input{})
}

var parser = adapter.New(config.Input{})
