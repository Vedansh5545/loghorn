// Package schema describes the fields of a log stream: their types, how to
// show them, how to convert raw record values to typed ones, and how to infer
// a first schema from the records themselves.
package schema

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Type is a field's data type.
type Type string

const (
	TInt       Type = "int"
	TFloat     Type = "float"
	TBool      Type = "bool"
	TString    Type = "string"
	TDuration  Type = "duration"
	TTimestamp Type = "timestamp"
	TJSON      Type = "json"
)

// Visualizer names how a column is rendered.
const (
	VRaw       = "raw"
	VNumber    = "number"
	VLocalTime = "local-time"
	VDuration  = "duration"
	VSeverity  = "severity"
	VSQL       = "sql"
	VJSON      = "json"
	VPlain     = "plain" // number shown without grouping separators
	VLink      = "link"  // whole value shown as one link
)

// Units valid for timestamp (epoch unit) and duration (display unit) fields.
const (
	UnitS  = "s"
	UnitMs = "ms"
	UnitUs = "us"
	UnitNs = "ns"
)

// ValidUnit reports whether unit is acceptable for a field of type t. The
// empty unit is valid for every type.
func ValidUnit(t Type, unit string) bool {
	switch unit {
	case "":
		return true
	case UnitS, UnitMs, UnitUs, UnitNs:
		return t == TTimestamp || t == TDuration
	}
	return false
}

// Field is one field of a stream schema.
type Field struct {
	Path       string `toml:"path" json:"path"`
	Type       Type   `toml:"type" json:"type"`
	Unit       string `toml:"unit,omitempty" json:"unit,omitempty"` // timestamp: s|ms|us|ns; duration: display unit
	Visualizer string `toml:"visualizer" json:"visualizer"`
	Label      string `toml:"label,omitempty" json:"label,omitempty"`
	Hidden     bool   `toml:"hidden,omitempty" json:"hidden,omitempty"`
	// Width is in pixels; 0 = automatic. BurntSushi/toml's omitempty does not
	// drop a zero int, so omitzero is what keeps `width = 0` out of saved files.
	Width int `toml:"width,omitempty,omitzero" json:"width,omitempty"`
	// Wrap shows the column's long values on several lines; false crops them.
	Wrap     bool `toml:"wrap,omitempty" json:"wrap,omitempty"`
	Inferred Type `toml:"-" json:"inferred"`
	Count    int  `toml:"-" json:"count"`
	New      bool `toml:"-" json:"new"`
	Saved    bool `toml:"-" json:"saved"`
}

// Schema is the field list and default columns of one stream.
type Schema struct {
	Stream string `toml:"stream" json:"stream"`
	// Source is record.SourceLog or record.SourceOtel; "" means log. Log
	// schemas save without it.
	Source  string   `toml:"source,omitempty" json:"source"`
	Columns []string `toml:"columns" json:"columns"`
	Fields  []Field  `toml:"field" json:"fields"` // sorted by Path
}

// Field returns the field with the given path.
func (s Schema) Field(path string) (Field, bool) {
	i := sort.Search(len(s.Fields), func(i int) bool { return s.Fields[i].Path >= path })
	if i < len(s.Fields) && s.Fields[i].Path == path {
		return s.Fields[i], true
	}
	for _, f := range s.Fields { // tolerate unsorted fields
		if f.Path == path {
			return f, true
		}
	}
	return Field{}, false
}

// Value is a record value converted to a field's type.
type Value struct {
	Kind  Type
	Int   int64   // exact for TInt
	Num   float64 // set for every numeric kind; duration = seconds
	Str   string
	Bool  bool
	Time  time.Time
	Empty bool // missing or null or ""
	Bad   bool // present but not convertible to Kind
}

// Convert converts a raw record value (string, json.Number, bool, nil, []any)
// to f's type. A value that does not fit is Bad, never silently coerced; a
// missing, null or empty one is Empty.
func Convert(v any, f Field) Value {
	out := Value{Kind: f.Type}
	switch x := v.(type) {
	case nil:
		out.Empty = true
		return out
	case string:
		if x == "" {
			out.Empty = true
			return out
		}
	}
	bad := func() Value { out.Bad = true; return out }

	switch f.Type {
	case TInt:
		var s string
		switch x := v.(type) {
		case json.Number:
			s = x.String()
		case string:
			s = strings.TrimSpace(x)
		default:
			return bad()
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return bad()
		}
		out.Int, out.Num = n, float64(n)
	case TFloat:
		var s string
		switch x := v.(type) {
		case json.Number:
			s = x.String()
		case string:
			s = strings.TrimSpace(x)
		default:
			return bad()
		}
		n, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return bad()
		}
		out.Num = n
	case TBool:
		switch x := v.(type) {
		case bool:
			out.Bool = x
		case string:
			b, err := strconv.ParseBool(strings.TrimSpace(x))
			if err != nil {
				return bad()
			}
			out.Bool = b
		default:
			return bad()
		}
	case TString:
		switch x := v.(type) {
		case string:
			out.Str = x
		case json.Number:
			out.Str = x.String()
		case bool:
			out.Str = strconv.FormatBool(x)
		default:
			b, err := json.Marshal(x)
			if err != nil {
				return bad()
			}
			out.Str = string(b)
		}
	case TDuration:
		switch x := v.(type) {
		case json.Number:
			n, err := x.Float64()
			if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
				return bad()
			}
			out.Num = n
		case string:
			sec, ok := ParseDuration(x)
			if !ok {
				return bad()
			}
			out.Num = sec
		default:
			return bad()
		}
	case TTimestamp:
		switch x := v.(type) {
		case string:
			t, err := time.Parse(time.RFC3339, strings.TrimSpace(x))
			if err != nil {
				return bad()
			}
			out.Time = t
		case json.Number:
			t, ok := epoch(x.String(), f.Unit)
			if !ok {
				return bad()
			}
			out.Time = t
		default:
			return bad()
		}
	case TJSON:
		if s, ok := v.(string); ok {
			out.Str = s
			break
		}
		b, err := json.Marshal(v)
		if err != nil {
			return bad()
		}
		out.Str = string(b)
	default:
		return bad()
	}
	return out
}

// epoch turns a numeric string in the given unit (default ms) into a time,
// keeping integers exact down to nanoseconds.
func epoch(s, unit string) (time.Time, bool) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		switch unit {
		case UnitS:
			return time.Unix(n, 0).UTC(), true
		case UnitUs:
			return time.UnixMicro(n).UTC(), true
		case UnitNs:
			return time.Unix(0, n).UTC(), true
		default:
			return time.UnixMilli(n).UTC(), true
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return time.Time{}, false
	}
	per := map[string]float64{UnitS: 1e9, UnitUs: 1e3, UnitNs: 1}[unit]
	if per == 0 {
		per = 1e6
	}
	ns := f * per
	if ns > math.MaxInt64 || ns < math.MinInt64 {
		return time.Time{}, false
	}
	return time.Unix(0, int64(ns)).UTC(), true
}

// ParseDuration parses Go duration syntax ("0.003s", "12ms", "1m30s") or a
// bare number, which means seconds.
func ParseDuration(s string) (seconds float64, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d.Seconds(), true
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, false
	}
	return n, true
}
