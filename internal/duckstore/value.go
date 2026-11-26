package duckstore

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/duckdb/duckdb-go/v2"
)

// SQLValue is the export value of a value DuckDB gives for a column: nil,
// string, int64, float64, bool, json.Number (numbers past int64: HUGEINT,
// UBIGINT, DECIMAL) or json.RawMessage (LIST, STRUCT, MAP). Times are RFC
// 3339 in UTC, BLOBs are text (bytes that are not UTF-8 as \xHH), and
// INTERVAL and UUID are written as DuckDB writes them.
func SQLValue(v any) any {
	switch x := v.(type) {
	case nil, string, int64, float64, bool:
		return x
	case int8:
		return int64(x)
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case int:
		return int64(x)
	case uint8:
		return int64(x)
	case uint16:
		return int64(x)
	case uint32:
		return int64(x)
	case uint64:
		if x <= math.MaxInt64 {
			return int64(x)
		}
		return json.Number(strconv.FormatUint(x, 10))
	case float32:
		return float64(x)
	case *big.Int:
		if x == nil {
			return nil
		}
		return json.Number(x.String())
	case duckdb.Decimal:
		return json.Number(decimalText(x))
	case duckdb.Interval:
		return intervalText(x)
	case duckdb.UUID:
		return x.String()
	case *duckdb.UUID:
		if x == nil {
			return nil
		}
		return x.String()
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	case []byte:
		return bytesText(x)
	case []any, map[string]any, duckdb.Map:
		b, err := json.Marshal(jsonable(x))
		if err != nil {
			return fmt.Sprint(x)
		}
		return json.RawMessage(b)
	}
	return fmt.Sprint(v)
}

// jsonable is v with every nested value as SQLValue gives it, maps keyed by
// text, and NaN and ±Inf as strings, so json.Marshal takes it.
func jsonable(v any) any {
	switch x := v.(type) {
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = jsonable(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = jsonable(e)
		}
		return out
	case duckdb.Map:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[fmt.Sprint(SQLValue(k))] = jsonable(e)
		}
		return out
	}
	switch y := SQLValue(v).(type) {
	case float64:
		if math.IsNaN(y) || math.IsInf(y, 0) {
			return strconv.FormatFloat(y, 'g', -1, 64)
		}
		return y
	default:
		return y
	}
}

// decimalText writes d with its scale's digits after the point.
func decimalText(d duckdb.Decimal) string {
	if d.Value == nil {
		return "0"
	}
	s := new(big.Int).Abs(d.Value).String()
	sign := ""
	if d.Value.Sign() < 0 {
		sign = "-"
	}
	sc := int(d.Scale)
	if sc == 0 {
		return sign + s
	}
	if len(s) <= sc {
		s = strings.Repeat("0", sc-len(s)+1) + s
	}
	return sign + s[:len(s)-sc] + "." + s[len(s)-sc:]
}

// intervalText writes an interval as DuckDB does: "1 month 2 days
// 03:04:05.5", leaving out parts that are zero.
func intervalText(iv duckdb.Interval) string {
	var parts []string
	plural := func(n int32, unit string) string {
		if n == 1 || n == -1 {
			return strconv.Itoa(int(n)) + " " + unit
		}
		return strconv.Itoa(int(n)) + " " + unit + "s"
	}
	if iv.Months/12 != 0 {
		parts = append(parts, plural(iv.Months/12, "year"))
	}
	if iv.Months%12 != 0 {
		parts = append(parts, plural(iv.Months%12, "month"))
	}
	if iv.Days != 0 {
		parts = append(parts, plural(iv.Days, "day"))
	}
	if iv.Micros != 0 || len(parts) == 0 {
		m := iv.Micros
		sign := ""
		if m < 0 {
			sign, m = "-", -m
		}
		frac := ""
		if us := m % 1e6; us != 0 {
			frac = strings.TrimRight(fmt.Sprintf(".%06d", us), "0")
		}
		parts = append(parts, fmt.Sprintf("%s%02d:%02d:%02d%s", sign, m/3600e6, m/60e6%60, m/1e6%60, frac))
	}
	return strings.Join(parts, " ")
}

// bytesText is b as text: itself when it is UTF-8, else each byte that is
// not printable ASCII as \xHH.
func bytesText(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	for _, c := range b {
		if c >= 0x20 && c < 0x7f && c != '\\' {
			sb.WriteByte(c)
		} else {
			fmt.Fprintf(&sb, `\x%02x`, c)
		}
	}
	return sb.String()
}
