package duckstore

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/adapter"
	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/schema"
)

// logRec normalizes one log line of stream with extra JSON members, the way
// the recorder does.
func logRec(t *testing.T, svc, stream, extra string) record.Record {
	t.Helper()
	line := fmt.Sprintf(`{"timestamp":"2026-11-23T10:00:00.123456789Z","severity":"INFO","message":"m","labels":{"schema":%q},"service":{"name":%q}%s}`, stream, svc, extra)
	r, ok := record.Normalize(adapter.New(config.Input{}).ParseLine([]byte(line)), config.Input{})
	if !ok {
		t.Fatalf("not a record: %s", line)
	}
	if r.Service == "" {
		r.Service = svc
	}
	return r
}

type rig struct {
	t   *testing.T
	e   *Engine
	seq int
}

func newRig(t *testing.T) *rig {
	t.Helper()
	e, err := OpenMemory(config.Input{}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return &rig{t: t, e: e}
}

func (g *rig) add(rs ...record.Record) []record.Record {
	for i := range rs {
		g.seq++
		rs[i].Seq = g.seq
		g.e.Add(rs[i])
	}
	if !g.e.Sync(5 * time.Second) {
		g.t.Fatal("sync timed out")
	}
	return rs
}

func (g *rig) one(q string, args ...any) any {
	g.t.Helper()
	var v any
	if err := g.e.DB().QueryRow(q, args...).Scan(&v); err != nil {
		g.t.Fatalf("%s: %v", q, err)
	}
	return v
}

// relation runs Relation for one field and returns (value, bad) per row in
// _id order.
func (g *rig) relation(src, svc, stream string, f schema.Field) (vals []any, bad []bool) {
	g.t.Helper()
	q, args := g.e.Relation(src, svc, stream, []schema.Field{f})
	rows, err := g.e.DB().Query("SELECT v0, x0 FROM ("+q+") ORDER BY _id", args...)
	if err != nil {
		g.t.Fatalf("%v\n%s", err, q)
	}
	defer rows.Close()
	for rows.Next() {
		var v any
		var x bool
		if err := rows.Scan(&v, &x); err != nil {
			g.t.Fatal(err)
		}
		vals, bad = append(vals, v), append(bad, x)
	}
	return vals, bad
}

func TestRecordsRoundTrip(t *testing.T) {
	g := newRig(t)
	in := g.add(
		logRec(t, "s1", "api", `,"requestId":7,"tags":["a","b"]`),
		logRec(t, "s1", "worker", `,"latency":"250ms"`),
		logRec(t, "s2", "api", `,"requestId":8.5`),
	)
	got, err := g.e.Records(context.Background(), "true")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(in) {
		t.Fatalf("got %d records, want %d", len(got), len(in))
	}
	for i := range in {
		w := in[i]
		if !reflect.DeepEqual(got[i], w) {
			t.Errorf("record %d:\n got %+v\nwant %+v", i, got[i], w)
		}
	}
	if n := g.one(`SELECT count(*) FROM all_records`); n != int64(3) {
		t.Errorf("all_records = %v", n)
	}
}

func TestTalliesGiveInferSchema(t *testing.T) {
	g := newRig(t)
	recs := g.add(
		logRec(t, "s1", "api", `,"n":7`),
		logRec(t, "s1", "api", `,"n":7.5,"x":null`),
		logRec(t, "s2", "api", `,"n":"seven"`),
	)
	for _, svc := range []string{"*", "s1", "s2"} {
		tl, err := g.e.Tallies(record.SourceLog, svc, "api")
		if err != nil {
			t.Fatal(err)
		}
		got := schema.FromTallies(record.SourceLog, "api", tl)
		want := schema.Infer(svc, record.SourceLog, "api", recs)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("service %s:\n got %+v\nwant %+v", svc, got, want)
		}
	}
}

func TestWideningKeepsTextAsReceived(t *testing.T) {
	g := newRig(t)
	g.add(logRec(t, "s", "api", `,"v":5`))
	if typ := g.colType("s", "api", "v"); typ != "BIGINT" {
		t.Fatalf("after int: %s", typ)
	}
	g.add(logRec(t, "s", "api", `,"v":1.50`))
	if typ := g.colType("s", "api", "v"); typ != "DOUBLE" {
		t.Fatalf("after float: %s", typ)
	}
	g.add(logRec(t, "s", "api", `,"v":"x"`))
	if typ := g.colType("s", "api", "v"); typ != "VARCHAR" {
		t.Fatalf("after string: %s", typ)
	}
	vals, _ := g.relation(record.SourceLog, "s", "api", schema.Field{Path: "v", Type: schema.TString})
	want := []any{"5", "1.50", "x"}
	if !reflect.DeepEqual(vals, want) {
		t.Errorf("values %v, want %v", vals, want)
	}
}

func (g *rig) colType(svc, table, col string) string {
	g.t.Helper()
	v := g.one(`SELECT data_type FROM duckdb_columns() WHERE database_name = 'lmem' AND schema_name = ? AND table_name = ? AND column_name = ?`, svc, table, col)
	s, _ := v.(string)
	return s
}

func TestBadValueIsNullAndFlagged(t *testing.T) {
	g := newRig(t)
	g.add(logRec(t, "s", "api", `,"receiveTimestamp":"soon"`), logRec(t, "s", "api", ``))
	vals, bad := g.relation(record.SourceLog, "s", "api", schema.Field{Path: "receiveTimestamp", Type: schema.TTimestamp})
	if vals[0] != nil || !bad[0] {
		t.Errorf("bad value: got (%v, %v), want (nil, true)", vals[0], bad[0])
	}
	if vals[1] != nil || bad[1] {
		t.Errorf("missing value: got (%v, %v), want (nil, false)", vals[1], bad[1])
	}
	recs, err := g.e.Records(context.Background(), "true")
	if err != nil {
		t.Fatal(err)
	}
	if raw := string(recs[0].Raw); !strings.Contains(raw, `"soon"`) {
		t.Errorf("_raw lost the value: %s", raw)
	}
}

func TestNamesDifferingInCaseStaySeparate(t *testing.T) {
	g := newRig(t)
	g.add(
		logRec(t, "s", "api", `,"userId":1,"a.b":"dot"`),
		logRec(t, "s", "api", `,"userID":2`),
		logRec(t, "S", "API", `,"userId":3`),
	)
	for _, c := range []struct {
		svc, stream, path string
		want              []any
	}{
		{"s", "api", "userId", []any{int64(1), nil}},
		{"s", "api", "userID", []any{nil, int64(2)}},
		{"s", "api", "a.b", []any{"dot", nil}},
		{"S", "API", "userId", []any{int64(3)}},
	} {
		vals, _ := g.relation(record.SourceLog, c.svc, c.stream, schema.Field{Path: c.path, Type: schema.TInt})
		if c.path == "a.b" {
			vals, _ = g.relation(record.SourceLog, c.svc, c.stream, schema.Field{Path: c.path, Type: schema.TString})
		}
		if !reflect.DeepEqual(vals, c.want) {
			t.Errorf("%s/%s %s = %v, want %v", c.svc, c.stream, c.path, vals, c.want)
		}
	}
}

// TestRelationConvertsLikeConvert checks every stored type against every
// effective type: the value and the bad flag must be what schema.Convert gives
// the value as received, except the known differences (D-011 #1).
func TestRelationConvertsLikeConvert(t *testing.T) {
	g := newRig(t)
	stored := map[string]string{ // service -> JSON value of f
		"int": `42`, "neg": `-7`, "float": `1.5`, "intfloat": `3.0`, "str": `"12"`, "strf": `" 2.5 "`,
		"word": `"hello"`, "bool": `true`, "strbool": `"T"`, "ts": `"2026-11-23T10:00:00.5Z"`,
		"dur": `"1m30s"`, "json": `[1,"a"]`, "epoch": `1700000000123`,
	}
	for svc, v := range stored {
		g.add(logRec(t, svc, "conv", `,"f":`+v))
	}
	types := []schema.Type{schema.TInt, schema.TFloat, schema.TBool, schema.TString, schema.TDuration, schema.TTimestamp, schema.TJSON}
	for svc, v := range stored {
		var raw any
		dec := json.NewDecoder(strings.NewReader(v))
		dec.UseNumber()
		if err := dec.Decode(&raw); err != nil {
			t.Fatal(err)
		}
		stype := schema.TallyType("f", schema.Classify(raw))
		for _, et := range types {
			if knownDifference(stype, et, raw) {
				continue
			}
			f := schema.Field{Path: "f", Type: et}
			want := schema.Convert(raw, f)
			vals, bad := g.relation(record.SourceLog, svc, "conv", f)
			if len(vals) != 1 {
				t.Fatalf("%s: %d rows", svc, len(vals))
			}
			if bad[0] != want.Bad {
				t.Errorf("%s (%s) as %s: bad = %v, want %v", svc, stype, et, bad[0], want.Bad)
				continue
			}
			if want.Bad || want.Empty {
				continue
			}
			if got, w := vals[0], expect(want, et); !reflect.DeepEqual(got, w) {
				t.Errorf("%s (%s) as %s: got %#v, want %#v", svc, stype, et, got, w)
			}
		}
	}
}

// knownDifference lists the stored → effective pairs where the original text
// is needed to match Convert, which typed columns don't keep (D-011 #1).
func knownDifference(stored, eff schema.Type, raw any) bool {
	if stored == eff {
		return false
	}
	switch {
	case eff == schema.TString || eff == schema.TJSON:
		return stored == schema.TFloat || stored == schema.TTimestamp || stored == schema.TDuration
	case stored == schema.TFloat && eff == schema.TInt:
		return true
	}
	return false
}

func expect(v schema.Value, t schema.Type) any {
	switch t {
	case schema.TInt:
		return v.Int
	case schema.TFloat, schema.TDuration:
		return v.Num
	case schema.TBool:
		return v.Bool
	case schema.TTimestamp:
		return v.Time.UTC()
	}
	return v.Str
}
