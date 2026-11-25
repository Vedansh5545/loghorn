package schema

// SQLType is the DuckDB type of a field of type t in the SQL table model,
// and the unit the model stores it in ("" when none). A duration is a
// DOUBLE in ms; an unknown or empty type is VARCHAR.
func SQLType(t Type) (sqlType string, unit string) {
	switch t {
	case TInt:
		return "BIGINT", ""
	case TFloat:
		return "DOUBLE", ""
	case TDuration:
		return "DOUBLE", UnitMs
	case TBool:
		return "BOOLEAN", ""
	case TTimestamp:
		return "TIMESTAMPTZ", ""
	case TJSON:
		return "JSON", ""
	}
	return "VARCHAR", ""
}
