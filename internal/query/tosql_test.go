package query

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/schema"
)

// sqlSchema has one field of every type, sorted by path as Schema needs.
var sqlSchema = schema.Schema{Stream: "app", Fields: []schema.Field{
	{Path: "a\"b", Type: schema.TString},
	{Path: "at", Type: schema.TTimestamp},
	{Path: "body", Type: schema.TJSON},
	{Path: "code", Type: schema.TInt},
	{Path: "duration_ms", Type: schema.TFloat},
	{Path: "latency", Type: schema.TDuration},
	{Path: "message", Type: schema.TString},
	{Path: "ok", Type: schema.TBool},
	{Path: "ratio", Type: schema.TFloat},
	{Path: "time", Type: schema.TString},
}}

// samplesRes is a run result whose columns the SQL selects.
func samplesRes(paths ...string) *Result {
	r := &Result{View: "samples"}
	for _, p := range paths {
		r.Columns = append(r.Columns, Column{Path: p})
	}
	return r
}

// toSQLBody runs ToSQL and returns what follows the header.
func toSQLBody(t *testing.T, q Query, r *Result) string {
	t.Helper()
	got, err := ToSQL(q, sqlSchema, r)
	if err != nil {
		t.Fatalf("ToSQL: %v", err)
	}
	if !strings.HasPrefix(got, SQLHeader) {
		t.Fatalf("no header:\n%s", got)
	}
	return strings.TrimPrefix(got, SQLHeader)
}

func TestToSQLHeader(t *testing.T) {
	got, err := ToSQL(Query{Stream: "app"}, sqlSchema, samplesRes("message"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"logs", "spans", `logs."app"`, `spans."*"`,
		"seq BIGINT", "time TIMESTAMPTZ", "service VARCHAR", "raw VARCHAR",
		"string VARCHAR", "int BIGINT", "float DOUBLE", "duration DOUBLE\n--   (ms)", "bool BOOLEAN",
		"time TIMESTAMPTZ", "json JSON", "loads as NULL", "not-empty",
		"loghorn.this_run_start()", "loghorn.this_run_id()",
	} {
		if !strings.Contains(SQLHeader, want) {
			t.Errorf("header lacks %q", want)
		}
	}
	want := SQLHeader + `SELECT "message"
FROM logs."app"
ORDER BY "time" DESC, seq DESC
LIMIT 100;
`
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestToSQLSamples(t *testing.T) {
	cases := []struct {
		name string
		q    Query
		r    *Result
		want string
	}{
		{"service", Query{Stream: "app", Service: "api"}, samplesRes("message", "code"),
			`SELECT "message", "code"
FROM logs."app"
WHERE service = 'api'
ORDER BY "time" DESC, seq DESC
LIMIT 100;
`},
		{"all services", Query{Stream: "app", Service: "*"}, samplesRes("message"),
			`SELECT "message"
FROM logs."app"
ORDER BY "time" DESC, seq DESC
LIMIT 100;
`},
		{"spans all kinds", Query{Stream: "*", Source: "otel"}, samplesRes("duration_ms"),
			`SELECT "duration_ms"
FROM spans."*"
ORDER BY "time" DESC, seq DESC
LIMIT 100;
`},
		{"no columns", Query{Stream: "http.server", Source: "otel"}, samplesRes(),
			`SELECT *
FROM spans."http.server"
ORDER BY "time" DESC, seq DESC
LIMIT 100;
`},
		{"query columns without result", Query{Stream: "app", Columns: []string{"ok"}}, nil,
			`SELECT "ok"
FROM logs."app"
ORDER BY "time" DESC, seq DESC
LIMIT 100;
`},
		{"sort field asc offset", Query{Stream: "app", Sort: Sort{Field: "code"}, Offset: 200}, samplesRes("code"),
			`SELECT "code"
FROM logs."app"
ORDER BY "code" ASC, seq ASC
LIMIT 100
OFFSET 200;
`},
		{"sort field desc limit capped", Query{Stream: "app", Sort: Sort{Field: "code", Desc: true}, Limit: 50000}, samplesRes("code"),
			`SELECT "code"
FROM logs."app"
ORDER BY "code" DESC, seq DESC
LIMIT 10000;
`},
		{"sort time asc", Query{Stream: "app", Sort: Sort{Field: "timestamp"}, Limit: 5}, samplesRes("code"),
			`SELECT "code"
FROM logs."app"
ORDER BY "time" ASC NULLS FIRST, seq ASC
LIMIT 5;
`},
		{"search", Query{Stream: "app", Search: "Ab%"}, samplesRes("message"),
			`SELECT "message"
FROM logs."app"
WHERE raw ILIKE '%ab\%%' ESCAPE '\'
ORDER BY "time" DESC, seq DESC
LIMIT 100;
`},
		{"injection", Query{Stream: "app", Filters: []Filter{{Field: "a\"b", Op: "=", Values: []string{"x' OR 1=1 --"}}}}, samplesRes("a\"b"),
			`SELECT "a""b"
FROM logs."app"
WHERE "a""b" = 'x'' OR 1=1 --'
ORDER BY "time" DESC, seq DESC
LIMIT 100;
`},
		{"long select list", Query{Stream: "app"}, samplesRes("message", "code", "ratio", "latency", "ok", "at", "body", "duration_ms", "nope"),
			`SELECT
  "message",
  "code",
  "ratio",
  "latency",
  "ok",
  "at",
  "body",
  "duration_ms",
  "nope"
FROM logs."app"
ORDER BY "time" DESC, seq DESC
LIMIT 100;
`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := toSQLBody(t, c.q, c.r); got != c.want {
				t.Errorf("got\n%s\nwant\n%s", got, c.want)
			}
		})
	}
}

// whereOf returns the WHERE clause of a samples statement over message.
func whereOf(t *testing.T, q Query) string {
	t.Helper()
	q.Stream = "app"
	body := toSQLBody(t, q, samplesRes("message"))
	body = strings.TrimPrefix(body, `SELECT "message"`+"\nFROM logs.\"app\"\n")
	i := strings.Index(body, "ORDER BY")
	if i < 0 {
		t.Fatalf("no ORDER BY: %s", body)
	}
	return body[:i]
}

func TestToSQLRange(t *testing.T) {
	cases := []struct {
		name string
		rg   Range
		want string
	}{
		{"relative hours", Range{From: "24h"}, "WHERE \"time\" >= now() - INTERVAL '24 hours'\n"},
		{"relative fraction", Range{From: "3.7h"}, "WHERE \"time\" >= now() - INTERVAL '13320 seconds'\n"},
		{"days as hours", Range{From: "1d"}, "WHERE \"time\" >= now() - INTERVAL '24 hours'\n"},
		{"milliseconds", Range{From: "1500ms"}, "WHERE \"time\" >= now() - INTERVAL '1500 milliseconds'\n"},
		{"absolute", Range{From: "2026-11-23T10:00:00+02:00", To: "2026-11-23T12:00:00.5Z"},
			"WHERE\n  \"time\" >= TIMESTAMPTZ '2026-11-23T08:00:00Z'\n  AND \"time\" <= TIMESTAMPTZ '2026-11-23T12:00:00.5Z'\n"},
		{"to only keeps timeless", Range{To: "2026-11-23T12:00:00Z"},
			"WHERE (\"time\" IS NULL OR \"time\" <= TIMESTAMPTZ '2026-11-23T12:00:00Z')\n"},
		{"all and now", Range{From: "all", To: "now"}, ""},
		{"empty", Range{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := whereOf(t, Query{Range: c.rg}); got != c.want {
				t.Errorf("got\n%s\nwant\n%s", got, c.want)
			}
		})
	}
	t.Run("this-run", func(t *testing.T) {
		body := toSQLBody(t, Query{Stream: "app", Range: Range{From: "this-run"}}, samplesRes("message"))
		want := thisRunNote + `SELECT "message"
FROM logs."app"
WHERE
  "time" >= loghorn.this_run_start()
  AND ("loghorn.run.id" = loghorn.this_run_id() OR "loghorn.run.id" IS NULL)
ORDER BY "time" DESC, seq DESC
LIMIT 100;
`
		if body != want {
			t.Errorf("got\n%s\nwant\n%s", body, want)
		}
		if !strings.Contains(thisRunNote, "no run ID") || !strings.Contains(thisRunNote, "untagged") {
			t.Errorf("note = %q", thisRunNote)
		}
	})
	t.Run("sub-microsecond is an error", func(t *testing.T) {
		if _, err := ToSQL(Query{Stream: "app", Range: Range{From: "2026-11-23T10:00:00.0000001Z"}}, sqlSchema, nil); err == nil {
			t.Error("want error")
		}
	})
}

func TestToSQLFilters(t *testing.T) {
	f := func(field, op string, vals ...string) Query {
		return Query{Filters: []Filter{{Field: field, Op: op, Values: vals}}}
	}
	cases := []struct {
		name string
		q    Query
		want string
	}{
		// string
		{"string =", f("message", "=", "hi"), `"message" = 'hi'`},
		{"string !=", f("message", "!=", "hi"), `("message" IS NULL OR "message" <> 'hi')`},
		{"string in", f("message", "in", "a", "b'c"), `"message" IN ('a', 'b''c')`},
		{"string not-in", f("message", "not-in", "a", "b"), `("message" IS NULL OR "message" NOT IN ('a', 'b'))`},
		{"string contains", f("message", "contains", `50%_Off\x`), `"message" ILIKE '%50\%\_off\\x%' ESCAPE '\'`},
		{"string empty", f("message", "empty"), `("message" IS NULL OR "message" = '')`},
		{"string not-empty", f("message", "not-empty"), `("message" IS NOT NULL AND "message" <> '')`},
		{"unknown field is a string", f("nope", "=", "x"), `"nope" = 'x'`},
		// int
		{"int =", f("code", "=", " 200"), `"code" = 200`},
		{"int !=", f("code", "!=", "200"), `("code" IS NULL OR "code" <> 200)`},
		{"int <", f("code", "<", "-3"), `"code" < -3`},
		{"int <=", f("code", "<=", "9223372036854775807"), `"code" <= 9223372036854775807`},
		{"int >", f("code", ">", "0"), `"code" > 0`},
		{"int >=", f("code", ">=", "500"), `"code" >= 500`},
		{"int empty", f("code", "empty"), `"code" IS NULL`},
		{"int not-empty", f("code", "not-empty"), `"code" IS NOT NULL`},
		// float
		{"float =", f("ratio", "=", "0.30000000000000004"), `"ratio" = 0.30000000000000004`},
		{"float !=", f("ratio", "!=", "1.5"), `("ratio" IS NULL OR "ratio" <> 1.5)`},
		{"float <", f("ratio", "<", "1e300"), `"ratio" < 1e+300`},
		{"float <=", f("ratio", "<=", "2"), `"ratio" <= 2`},
		{"float >", f("ratio", ">", "-0.5"), `"ratio" > -0.5`},
		{"float >=", f("ratio", ">=", "0"), `"ratio" >= 0`},
		{"float empty", f("ratio", "empty"), `"ratio" IS NULL`},
		{"float not-empty", f("ratio", "not-empty"), `"ratio" IS NOT NULL`},
		// duration: the model stores ms
		{"duration =", f("latency", "=", "250ms"), `"latency" = 250`},
		{"duration !=", f("latency", "!=", "1.5s"), `("latency" IS NULL OR "latency" <> 1500)`},
		{"duration <", f("latency", "<", "0.1ms"), `"latency" < 0.1`},
		{"duration <=", f("latency", "<=", "2s"), `"latency" <= 2000`},
		{"duration >", f("latency", ">", "1m"), `"latency" > 60000`},
		{"duration >=", f("latency", ">=", "87ms"), `"latency" >= 87`},
		{"duration empty", f("latency", "empty"), `"latency" IS NULL`},
		{"duration not-empty", f("latency", "not-empty"), `"latency" IS NOT NULL`},
		// timestamp
		{"timestamp =", f("at", "=", "2026-11-23T10:00:00Z"), `"at" = TIMESTAMPTZ '2026-11-23T10:00:00Z'`},
		{"timestamp !=", f("at", "!=", "2026-11-23T10:00:00Z"), `("at" IS NULL OR "at" <> TIMESTAMPTZ '2026-11-23T10:00:00Z')`},
		{"timestamp <", f("at", "<", "2026-11-23T10:00:00-05:00"), `"at" < TIMESTAMPTZ '2026-11-23T15:00:00Z'`},
		{"timestamp <=", f("at", "<=", "2026-11-23T10:00:00.25Z"), `"at" <= TIMESTAMPTZ '2026-11-23T10:00:00.25Z'`},
		{"timestamp >", f("at", ">", "2026-11-23T10:00:00Z"), `"at" > TIMESTAMPTZ '2026-11-23T10:00:00Z'`},
		{"timestamp >=", f("at", ">=", "2026-11-23T10:00:00Z"), `"at" >= TIMESTAMPTZ '2026-11-23T10:00:00Z'`},
		{"timestamp empty", f("at", "empty"), `"at" IS NULL`},
		{"timestamp not-empty", f("at", "not-empty"), `"at" IS NOT NULL`},
		// bool
		{"bool =", f("ok", "=", "true"), `"ok" = true`},
		{"bool = false", f("ok", "=", "0"), `"ok" = false`},
		{"bool empty", f("ok", "empty"), `"ok" IS NULL`},
		{"bool not-empty", f("ok", "not-empty"), `"ok" IS NOT NULL`},
		// json
		{"json contains", f("body", "contains", `"Id":`), `"body" ILIKE '%"id":%' ESCAPE '\'`},
		{"json empty", f("body", "empty"), `"body" IS NULL`},
		{"json not-empty", f("body", "not-empty"), `"body" IS NOT NULL`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got, want := whereOf(t, c.q), "WHERE "+c.want+"\n"; got != want {
				t.Errorf("got\n%s\nwant\n%s", got, want)
			}
		})
	}
	// Every operator of every type has a case above.
	seen := map[string]bool{}
	for _, c := range cases {
		fl := c.q.Filters[0]
		seen[string(fieldType(sqlSchema, fl.Field))+" "+fl.Op] = true
	}
	for _, typ := range []schema.Type{schema.TString, schema.TInt, schema.TFloat, schema.TDuration, schema.TTimestamp, schema.TBool, schema.TJSON} {
		for _, op := range OpsFor(typ) {
			if !seen[string(typ)+" "+op] {
				t.Errorf("no case for %s %s", typ, op)
			}
		}
	}
}

func TestToSQLCombined(t *testing.T) {
	q := Query{Service: "api", Range: Range{From: "1h"}, Search: "boom",
		Filters: []Filter{{Field: "code", Op: ">=", Values: []string{"500"}}, {Field: "message", Op: "not-empty"}}}
	want := `WHERE
  service = 'api'
  AND "time" >= now() - INTERVAL '1 hours'
  AND raw ILIKE '%boom%' ESCAPE '\'
  AND "code" >= 500
  AND ("message" IS NOT NULL AND "message" <> '')
`
	if got := whereOf(t, q); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestToSQLCell(t *testing.T) {
	lo := 1.5
	from := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	q := Query{Source: "otel", Cell: &HeatCell{From: from, To: from.Add(time.Minute), Lo: &lo, Hi: 2.5}}
	want := `WHERE
  "time" >= TIMESTAMPTZ '2026-11-23T10:00:00Z'
  AND "time" < TIMESTAMPTZ '2026-11-23T10:01:00Z'
  AND "duration_ms" >= 1.5
  AND "duration_ms" < 2.5
`
	body := toSQLBody(t, Query{Stream: "app", Source: q.Source, Cell: q.Cell}, samplesRes("message"))
	if !strings.Contains(body, want) {
		t.Errorf("got\n%s\nwant\n%s", body, want)
	}
	q.Cell.Lo = nil
	body = toSQLBody(t, Query{Stream: "app", Source: q.Source, Cell: q.Cell}, samplesRes("message"))
	if strings.Contains(body, `"duration_ms" >=`) || !strings.Contains(body, `"duration_ms" < 2.5`) {
		t.Errorf("no lo:\n%s", body)
	}
}

func TestToSQLErrors(t *testing.T) {
	cases := []struct {
		name string
		q    Query
		r    *Result
	}{
		{"model column in columns", Query{Stream: "app"}, samplesRes("time")},
		{"model column any case", Query{Stream: "app"}, samplesRes("Service")},
		{"model column in filter", Query{Stream: "app", Filters: []Filter{{Field: "raw", Op: "empty"}}}, samplesRes("message")},
		{"model column in sort", Query{Stream: "app", Sort: Sort{Field: "seq"}}, samplesRes("message")},
		{"NUL in value", Query{Stream: "app", Filters: []Filter{{Field: "message", Op: "=", Values: []string{"a\x00b"}}}}, samplesRes("message")},
		{"infinite float", Query{Stream: "app", Filters: []Filter{{Field: "ratio", Op: "<", Values: []string{"inf"}}}}, samplesRes("message")},
		{"invalid query", Query{Stream: "app", Filters: []Filter{{Field: "code", Op: "contains", Values: []string{"1"}}}}, samplesRes("message")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got, err := ToSQL(c.q, sqlSchema, c.r); err == nil {
				t.Errorf("want error, got\n%s", got)
			}
		})
	}
	for _, v := range []string{"table", "graph"} {
		if _, err := ToSQL(Query{Stream: "app", View: v}, sqlSchema, &Result{View: v}); !errors.Is(err, ErrSQLView) {
			t.Errorf("view %s: err = %v, want ErrSQLView", v, err)
		}
	}
}
