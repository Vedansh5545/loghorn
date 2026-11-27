package duckstore

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/jimbeveridge/loghorn/internal/schema"
)

// TestDurationSQLMatchesGo: durationSQL gives exactly the float64 that
// schema.ParseDuration does, which uses time.ParseDuration's whole
// nanoseconds, for every unit and for compound, fractional, signed,
// overflowing and bare-number values (review §2.1).
func TestDurationSQLMatchesGo(t *testing.T) {
	var in []string
	for n := 1; n <= 5000; n++ {
		for _, u := range []string{"ns", "us", "µs", "μs", "ms", "s", "m", "h"} {
			in = append(in, fmt.Sprintf("%d%s", n, u))
		}
		in = append(in, fmt.Sprintf("0.%03ds", n%1000), fmt.Sprintf("%d.%dms", n, n%7), fmt.Sprintf("-%dus", n))
	}
	in = append(in, "0.1s0.2s", "1h2m3.5s", "1.5h", "-1.5h", "+2m", "2h45m0.5s7ns", ".5s", "5.s",
		"0.123456789123456789s", "1.00000000000000000001s", "0.0000000005s", "1.9999999999s",
		"2562047h47m16.854775807s", "2562047h47m16.854775808s", "-2562047h47m16.854775808s", "9999999999h",
		"0", "-0", "-0s", "3", "2.5", "1e3", " 7ms ", "\t1m30s\n", "5", "1d", "s", "-", "", "inf", "NaN")
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows := make([]string, len(in))
	for i, s := range in {
		rows[i] = "(" + fmt.Sprint(i) + ", " + lit(s) + ")"
	}
	q := "SELECT i, " + durationSQL(trimSQL("s")) + " FROM (VALUES " + strings.Join(rows, ",") + ") t(i, s) ORDER BY i"
	res, err := db.Query(q)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Close()
	bad := 0
	for res.Next() {
		var i int
		var got sql.NullFloat64
		if err := res.Scan(&i, &got); err != nil {
			t.Fatal(err)
		}
		want, ok := schema.ParseDuration(in[i])
		if got.Valid != ok || (ok && (got.Float64 != want || (want == 0 && fmt.Sprint(got.Float64) != "0"))) {
			if bad++; bad <= 10 {
				t.Errorf("%q: SQL %v (valid %v), Go %v (ok %v)", in[i], got.Float64, got.Valid, want, ok)
			}
		}
	}
	if bad > 0 {
		t.Errorf("%d of %d durations differ", bad, len(in))
	}
}
