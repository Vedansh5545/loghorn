package duckstore

import (
	"encoding/json"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/duckdb/duckdb-go/v2"
)

func TestSQLValue(t *testing.T) {
	when := time.Date(2026, 11, 25, 10, 0, 1, 500, time.FixedZone("x", -6*3600))
	cases := []struct {
		in   any
		want any
	}{
		{nil, nil},
		{"a", "a"},
		{int32(7), int64(7)},
		{int64(-3), int64(-3)},
		{uint64(18446744073709551615), json.Number("18446744073709551615")},
		{big.NewInt(42), json.Number("42")},
		{float32(0.5), float64(0.5)},
		{math.NaN(), math.NaN()},
		{true, true},
		{when, "2026-11-25T16:00:01.0000005Z"},
		{[]byte("text"), "text"},
		{[]byte{0xff, 0x00}, `\xff\x00`},
		{[]any{int64(1), "b", nil}, json.RawMessage(`[1,"b",null]`)},
		{map[string]any{"k": int64(1)}, json.RawMessage(`{"k":1}`)},
		{duckdb.Decimal{Width: 5, Scale: 2, Value: big.NewInt(12345)}, json.Number("123.45")},
		{duckdb.Interval{Days: 1, Micros: 1500}, "1 day 00:00:00.0015"},
		{duckdb.UUID{0x12, 0x34}, "12340000-0000-0000-0000-000000000000"},
	}
	for _, c := range cases {
		got := SQLValue(c.in)
		if f, ok := c.want.(float64); ok && math.IsNaN(f) {
			if g, ok := got.(float64); !ok || !math.IsNaN(g) {
				t.Errorf("SQLValue(%v) = %#v, want NaN", c.in, got)
			}
			continue
		}
		gb, _ := json.Marshal(got)
		wb, _ := json.Marshal(c.want)
		if string(gb) != string(wb) || (got == nil) != (c.want == nil) {
			t.Errorf("SQLValue(%#v) = %#v, want %#v", c.in, got, c.want)
		}
	}
}
