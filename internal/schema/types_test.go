package schema

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	for _, c := range []struct {
		in   string
		want float64
		ok   bool
	}{{"0.003s", 0.003, true}, {"12ms", 0.012, true}, {"2", 2, true}, {"1m30s", 90, true}, {"fast", 0, false}, {"", 0, false}, {"NaN", 0, false}} {
		got, ok := ParseDuration(c.in)
		if ok != c.ok || (ok && math.Abs(got-c.want) > 1e-9) {
			t.Errorf("ParseDuration(%q)=%v,%v want %v,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestConvertIntExact(t *testing.T) {
	v := Convert(json.Number("1763400000123456789"), Field{Type: TInt})
	if v.Bad || v.Int != 1763400000123456789 {
		t.Fatalf("%+v", v)
	}
}

func TestConvertTimestampUnits(t *testing.T) {
	ms := Convert(json.Number("1763400000123"), Field{Type: TTimestamp, Unit: "ms"})
	def := Convert(json.Number("1763400000123"), Field{Type: TTimestamp})
	ns := Convert(json.Number("1763400000123456789"), Field{Type: TTimestamp, Unit: "ns"})
	us := Convert(json.Number("1763400000123456"), Field{Type: TTimestamp, Unit: "us"})
	s := Convert(json.Number("1763400000"), Field{Type: TTimestamp, Unit: "s"})
	if ms.Bad || ns.Bad || !ms.Time.Equal(ns.Time.Truncate(time.Millisecond)) || !ms.Time.Equal(def.Time) {
		t.Fatalf("ms=%v ns=%v def=%v", ms.Time, ns.Time, def.Time)
	}
	if !us.Time.Truncate(time.Millisecond).Equal(ms.Time) || !s.Time.Equal(ms.Time.Truncate(time.Second)) {
		t.Fatalf("us=%v s=%v", us.Time, s.Time)
	}
	if ns.Time.Nanosecond() != 123456789 {
		t.Errorf("ns not exact: %d", ns.Time.Nanosecond())
	}
	rfc := Convert("2026-11-17T17:00:01.123Z", Field{Type: TTimestamp})
	if rfc.Bad || !rfc.Time.Equal(time.Date(2026, 11, 17, 17, 0, 1, 123000000, time.UTC)) {
		t.Errorf("rfc: %+v", rfc)
	}
}

func TestConvertBad(t *testing.T) {
	v := Convert("n/a", Field{Type: TInt})
	if !v.Bad || v.Empty {
		t.Fatalf("%+v", v)
	}
	for _, c := range []struct {
		v any
		t Type
	}{{"x", TFloat}, {"x", TBool}, {"x", TDuration}, {"x", TTimestamp}, {json.Number("1.5"), TInt}, {true, TInt}} {
		if got := Convert(c.v, Field{Type: c.t}); !got.Bad {
			t.Errorf("Convert(%v,%s) not Bad: %+v", c.v, c.t, got)
		}
	}
}

func TestConvertEmpty(t *testing.T) {
	for _, ty := range []Type{TInt, TFloat, TBool, TString, TDuration, TTimestamp, TJSON} {
		for _, v := range []any{nil, ""} {
			if got := Convert(v, Field{Type: ty}); !got.Empty || got.Bad {
				t.Errorf("Convert(%v,%s)=%+v", v, ty, got)
			}
		}
	}
}

func TestConvertOK(t *testing.T) {
	if v := Convert("3.2ms", Field{Type: TDuration}); v.Bad || math.Abs(v.Num-0.0032) > 1e-9 {
		t.Errorf("duration %+v", v)
	}
	if v := Convert(json.Number("2.5"), Field{Type: TFloat}); v.Num != 2.5 {
		t.Errorf("float %+v", v)
	}
	if v := Convert(true, Field{Type: TBool}); !v.Bool || v.Bad {
		t.Errorf("bool %+v", v)
	}
	if v := Convert(json.Number("7"), Field{Type: TString}); v.Str != "7" {
		t.Errorf("string %+v", v)
	}
	if v := Convert([]any{"a", json.Number("1")}, Field{Type: TJSON}); v.Str != `["a",1]` {
		t.Errorf("json %+v", v)
	}
}

func TestSchemaField(t *testing.T) {
	s := Schema{Fields: []Field{{Path: "a"}, {Path: "b", Type: TInt}}}
	if f, ok := s.Field("b"); !ok || f.Type != TInt {
		t.Error("b")
	}
	if _, ok := s.Field("c"); ok {
		t.Error("c")
	}
}

func TestValidUnit(t *testing.T) {
	if !ValidUnit(TTimestamp, "ns") || !ValidUnit(TDuration, "ms") || !ValidUnit(TInt, "") || ValidUnit(TInt, "ms") || ValidUnit(TTimestamp, "h") {
		t.Error("ValidUnit")
	}
}
