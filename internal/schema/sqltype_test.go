package schema

import "testing"

func TestSQLType(t *testing.T) {
	cases := []struct {
		in        Type
		typ, unit string
	}{
		{TString, "VARCHAR", ""},
		{TInt, "BIGINT", ""},
		{TFloat, "DOUBLE", ""},
		{TDuration, "DOUBLE", "ms"},
		{TBool, "BOOLEAN", ""},
		{TTimestamp, "TIMESTAMPTZ", ""},
		{TJSON, "JSON", ""},
		{"", "VARCHAR", ""},
		{"weird", "VARCHAR", ""},
	}
	for _, c := range cases {
		typ, unit := SQLType(c.in)
		if typ != c.typ || unit != c.unit {
			t.Errorf("SQLType(%q) = %q, %q; want %q, %q", c.in, typ, unit, c.typ, c.unit)
		}
	}
}
