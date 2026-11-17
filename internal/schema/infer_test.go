package schema

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/jimbeveridge/loghorn/internal/record"
)

func rec(stream string, f map[string]any) record.Record {
	return record.Record{Stream: stream, Fields: f}
}

func typeOf(t *testing.T, s Schema, path string) Type {
	t.Helper()
	f, ok := s.Field(path)
	if !ok {
		t.Fatalf("no field %q", path)
	}
	return f.Type
}

func TestInferTypes(t *testing.T) {
	recs := []record.Record{
		rec("db", map[string]any{
			"timestamp": "2026-11-17T17:00:01.123Z", "message": "query ok",
			"databaseContext.metadata.rowsAffected": json.Number("0"),
			"databaseContext.metadata.latency":      "0.4ms",
			"ok":                                    true, "tags": []any{"a"}, "ratio": json.Number("1.5"),
		}),
		rec("db", map[string]any{
			"timestamp": "2026-11-17T17:00:02.123Z", "message": "slow",
			"databaseContext.metadata.rowsAffected": json.Number("42"),
			"databaseContext.metadata.latency":      "87ms",
			"ok":                                    false, "tags": []any{}, "ratio": json.Number("2"),
		}),
		rec("other", map[string]any{"message": 5}),
	}
	s := Infer("db", recs)
	want := map[string]Type{
		"databaseContext.metadata.rowsAffected": TInt, "databaseContext.metadata.latency": TDuration,
		"timestamp": TTimestamp, "message": TString, "ok": TBool, "tags": TJSON, "ratio": TFloat,
	}
	for p, ty := range want {
		if got := typeOf(t, s, p); got != ty {
			t.Errorf("%s=%s want %s", p, got, ty)
		}
	}
	f, _ := s.Field("databaseContext.metadata.latency")
	if f.Count != 2 || f.Inferred != TDuration || f.Visualizer != VDuration {
		t.Errorf("%+v", f)
	}
	for i := 1; i < len(s.Fields); i++ {
		if s.Fields[i-1].Path >= s.Fields[i].Path {
			t.Fatal("fields not sorted")
		}
	}
}

func TestInferMixedIsString(t *testing.T) {
	s := Infer("a", []record.Record{
		rec("a", map[string]any{"rows": json.Number("3")}),
		rec("a", map[string]any{"rows": "n/a"}),
	})
	if got := typeOf(t, s, "rows"); got != TString {
		t.Fatalf("rows=%s", got)
	}
}

func TestInferNumberNeverTimestamp(t *testing.T) {
	s := Infer("a", []record.Record{rec("a", map[string]any{"at": json.Number("1763400000123"), "bare": "42"})})
	if got := typeOf(t, s, "at"); got != TInt {
		t.Errorf("at=%s", got)
	}
	if got := typeOf(t, s, "bare"); got != TString {
		t.Errorf("bare=%s", got)
	}
	// the well-known timestamp fields are timestamps whatever they hold
	s = Infer("a", []record.Record{rec("a", map[string]any{"receiveTimestamp": json.Number("1")})})
	if got := typeOf(t, s, "receiveTimestamp"); got != TTimestamp {
		t.Errorf("receiveTimestamp=%s", got)
	}
}

func TestInferNullsIgnored(t *testing.T) {
	s := Infer("a", []record.Record{
		rec("a", map[string]any{"n": nil}), rec("a", map[string]any{"n": json.Number("3")}), rec("a", map[string]any{"n": ""}),
	})
	f, _ := s.Field("n")
	if f.Type != TInt || f.Count != 3 {
		t.Errorf("%+v", f)
	}
}

func TestDefaultColumns(t *testing.T) {
	f := map[string]any{"timestamp": "2026-11-17T17:00:01Z", "severity": "INFO", "message": "m", "labels.schema": "a", "tags": []any{"x"}}
	for i := 0; i < 10; i++ {
		f[fmt.Sprintf("f%02d", i)] = "v"
	}
	r1 := rec("a", f)
	r2 := rec("a", map[string]any{"f09": "v", "f08": "v"})
	s := Infer("a", []record.Record{r1, r2})
	want := []string{"timestamp", "severity", "message", "f08", "f09", "f00", "f01", "f02"}
	if !reflect.DeepEqual(s.Columns, want) {
		t.Fatalf("columns=%v want %v", s.Columns, want)
	}
	// absent well-known columns are skipped
	s = Infer("a", []record.Record{rec("a", map[string]any{"x": "1"})})
	if !reflect.DeepEqual(s.Columns, []string{"x"}) {
		t.Errorf("columns=%v", s.Columns)
	}
}

func TestDefaultVisualizer(t *testing.T) {
	for _, c := range []struct {
		p string
		t Type
		w string
	}{{"x", TInt, VNumber}, {"x", TFloat, VNumber}, {"x", TDuration, VDuration}, {"x", TTimestamp, VLocalTime}, {"x", TJSON, VJSON}, {"severity", TString, VSeverity}, {"x", TString, VRaw}, {"x", TBool, VRaw}} {
		if got := DefaultVisualizer(c.p, c.t); got != c.w {
			t.Errorf("%s/%s=%s want %s", c.p, c.t, got, c.w)
		}
	}
}
