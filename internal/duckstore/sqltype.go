package duckstore

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/jimbeveridge/loghorn/internal/schema"
)

// sqlType is the DuckDB column type of a loghorn type (D-014, D-017).
func sqlType(t schema.Type) string {
	switch t {
	case schema.TInt:
		return "BIGINT"
	case schema.TFloat, schema.TDuration:
		return "DOUBLE"
	case schema.TBool:
		return "BOOLEAN"
	case schema.TTimestamp:
		return "TIMESTAMP_NS"
	}
	return "VARCHAR" // string, json, and an unset type
}

// norm treats an unset type as string, as query.lookupField does.
func norm(t schema.Type) schema.Type {
	if t == "" {
		return schema.TString
	}
	return t
}

// widest is the type of a view column holding columns of types a and b.
func widest(a, b schema.Type) schema.Type {
	a, b = norm(a), norm(b)
	switch {
	case a == b:
		return a
	case (a == schema.TInt && b == schema.TFloat) || (a == schema.TFloat && b == schema.TInt):
		return schema.TFloat
	}
	return schema.TString
}

// ident quotes an identifier.
func ident(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// lit quotes a string literal.
func lit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// flit is a DOUBLE literal that reads back as exactly f.
func flit(f float64) string {
	return "CAST(" + lit(strconv.FormatFloat(f, 'g', -1, 64)) + " AS DOUBLE)"
}

func nullOf(t schema.Type) string { return "CAST(NULL AS " + sqlType(t) + ")" }

const (
	rfc3339Re = `^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})$`
	durPart   = `([0-9]+\.?[0-9]*|\.[0-9]+)(ns|us|µs|μs|ms|s|m|h)`
)

// conv is the SQL giving schema.Convert's value for a column c stored as
// type from, read as field to. NULL means empty or not convertible; callers
// tell the two apart with bad. Where the original text would be needed
// (D-011 #1) it gives DuckDB's cast instead.
func conv(c string, from schema.Type, to schema.Field) string {
	from, t := norm(from), norm(to.Type)
	if from == t {
		return c
	}
	num := from == schema.TInt || from == schema.TFloat
	switch t {
	case schema.TString, schema.TJSON:
		if from == schema.TJSON || from == schema.TString {
			return c
		}
		return "CAST(" + c + " AS VARCHAR)"
	case schema.TInt:
		switch {
		case from == schema.TFloat:
			return "(CASE WHEN " + c + " = trunc(" + c + ") AND abs(" + c + ") < 9.2e18 THEN CAST(" + c + " AS BIGINT) END)"
		case from == schema.TString:
			return "(CASE WHEN regexp_full_match(trim(" + c + "), '[+-]?[0-9]+') THEN TRY_CAST(trim(" + c + ") AS BIGINT) END)"
		}
	case schema.TFloat:
		switch {
		case from == schema.TInt:
			return "CAST(" + c + " AS DOUBLE)"
		case from == schema.TString:
			return "(CASE WHEN isfinite(TRY_CAST(trim(" + c + ") AS DOUBLE)) THEN TRY_CAST(trim(" + c + ") AS DOUBLE) END)"
		}
	case schema.TBool:
		if from == schema.TString {
			return "(CASE WHEN trim(" + c + ") IN ('1','t','T','TRUE','true','True') THEN true WHEN trim(" + c + ") IN ('0','f','F','FALSE','false','False') THEN false END)"
		}
	case schema.TDuration:
		switch {
		case num:
			return "CAST(" + c + " AS DOUBLE)"
		case from == schema.TString:
			return durationSQL("trim(" + c + ")")
		}
	case schema.TTimestamp:
		switch {
		case from == schema.TInt:
			return epochIntSQL(c, to.Unit)
		case from == schema.TFloat:
			return "(CASE WHEN " + c + " = trunc(" + c + ") AND abs(" + c + ") < 9.2e18 THEN " + epochIntSQL("CAST("+c+" AS BIGINT)", to.Unit) + " ELSE " + epochFloatSQL(c, to.Unit) + " END)"
		case from == schema.TString:
			s := "trim(" + c + ")"
			return "(CASE WHEN regexp_full_match(" + s + ", " + lit(rfc3339Re) + ") THEN CAST(TRY_CAST(" + s + " AS TIMESTAMPTZ) AT TIME ZONE 'UTC' AS TIMESTAMP_NS) END)"
		}
	}
	return nullOf(t)
}

var (
	numericKinds = schema.Classify(json.Number("1")) | schema.Classify(json.Number("1.5"))
	noneKind     = schema.Classify(nil)
)

// convCol is conv for a table column, using the kinds of value it holds: a
// timestamp column holding only numbers stored them as epoch ms (the
// inferred field has no unit), so a saved unit rescales them as
// schema.Convert would read the numbers. A column mixing numbers and text
// can't tell its rows apart and keeps ms (D-011 #1).
func convCol(c string, col column, to schema.Field) string {
	k := col.kinds &^ noneKind
	if norm(col.typ) == schema.TTimestamp && norm(to.Type) == schema.TTimestamp && k != 0 && k&^numericKinds == 0 {
		ns := "epoch_ns(" + c + ")"
		switch to.Unit {
		case schema.UnitS:
			return "(CASE WHEN abs(" + ns + ") <= 9223372036854775 THEN make_timestamp_ns(" + ns + " * 1000) END)"
		case schema.UnitUs:
			return "make_timestamp_ns(" + ns + " // 1000)"
		case schema.UnitNs:
			return "make_timestamp_ns(" + ns + " // 1000000)"
		}
	}
	return conv(c, col.typ, to)
}

// durationSQL parses Go duration syntax or a bare number (seconds), as
// schema.ParseDuration does.
func durationSQL(s string) string {
	units := "CASE regexp_extract(x, '[a-zµμ]+$') WHEN 'ns' THEN 1e-9 WHEN 'us' THEN 1e-6 WHEN 'µs' THEN 1e-6 WHEN 'μs' THEN 1e-6 WHEN 'ms' THEN 1e-3 WHEN 's' THEN 1.0 WHEN 'm' THEN 60.0 WHEN 'h' THEN 3600.0 END"
	sum := "list_sum(list_transform(regexp_extract_all(" + s + ", " + lit(durPart) + ", 0), lambda x: CAST(regexp_extract(x, '^[0-9.]+') AS DOUBLE) * (" + units + ")))"
	return "(CASE WHEN isfinite(TRY_CAST(" + s + " AS DOUBLE)) THEN TRY_CAST(" + s + " AS DOUBLE)" +
		" WHEN regexp_full_match(" + s + ", " + lit(`[-+]?(`+durPart+`)+`) + ") THEN (CASE WHEN starts_with(" + s + ", '-') THEN -1.0 ELSE 1.0 END) * " + sum + " END)"
}

// epochIntSQL is schema's epoch() for an integer in unit (default ms).
func epochIntSQL(c, unit string) string {
	mul, limit := "1000000", "9223372036854"
	switch unit {
	case schema.UnitS:
		mul, limit = "1000000000", "9223372036"
	case schema.UnitUs:
		mul, limit = "1000", "9223372036854775"
	case schema.UnitNs:
		return "make_timestamp_ns(" + c + ")"
	}
	return "(CASE WHEN abs(" + c + ") <= " + limit + " THEN make_timestamp_ns(" + c + " * " + mul + ") END)"
}

// epochFloatSQL is schema's epoch() for a non-integer number in unit.
func epochFloatSQL(c, unit string) string {
	per := "1e6"
	switch unit {
	case schema.UnitS:
		per = "1e9"
	case schema.UnitUs:
		per = "1e3"
	case schema.UnitNs:
		per = "1.0"
	}
	ns := "(" + c + " * " + per + ")"
	return "(CASE WHEN abs(" + ns + ") < 9.2e18 THEN make_timestamp_ns(CAST(trunc(" + ns + ") AS BIGINT)) END)"
}
