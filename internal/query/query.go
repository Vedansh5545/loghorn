// Package query runs typed filters, a time range, a text search and a sort
// over the records of one stream.
package query

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jimbeveridge/loghorn/internal/schema"
)

// Row limits.
const (
	DefaultLimit = 1000
	MaxLimit     = 10000
)

// Range is the time window of a query.
type Range struct {
	From string `json:"from" toml:"from"`
	To   string `json:"to" toml:"to"`
}

// Filter is one field condition; filters combine with AND.
type Filter struct {
	Field  string   `json:"field" toml:"field"`
	Op     string   `json:"op" toml:"op"`
	Values []string `json:"values" toml:"values"`
}

// Sort orders the result.
type Sort struct {
	Field string `json:"field" toml:"field"`
	Desc  bool   `json:"desc" toml:"desc"`
}

// Aggregate is reserved for a later aggregation stage.
type Aggregate struct {
	Op    string `json:"op" toml:"op"`
	Field string `json:"field,omitempty" toml:"field,omitempty"`
}

// Query is the saved/serialised query model.
type Query struct {
	Stream  string   `json:"stream" toml:"stream"`
	Range   Range    `json:"range" toml:"range"`
	Filters []Filter `json:"filters" toml:"filters"`
	Search  string   `json:"search" toml:"search"`
	Columns []string `json:"columns" toml:"columns"`
	Sort    Sort     `json:"sort" toml:"sort"`
	Limit   int      `json:"limit" toml:"limit"`

	// Reserved for aggregation; accepted, stored, ignored by Run in v1.
	GroupBy    []string    `json:"groupBy,omitempty" toml:"groupBy,omitempty"`
	Aggregates []Aggregate `json:"aggregates,omitempty" toml:"aggregates,omitempty"`
	Bucket     string      `json:"bucket,omitempty" toml:"bucket,omitempty"`
	View       string      `json:"view,omitempty" toml:"view,omitempty"`
}

// OpsFor lists the operators valid for a field type.
func OpsFor(t schema.Type) []string {
	var ops []string
	switch t {
	case schema.TInt, schema.TFloat, schema.TDuration, schema.TTimestamp:
		ops = []string{"=", "!=", "<", "<=", ">", ">="}
	case schema.TString:
		ops = []string{"=", "!=", "in", "not-in", "contains"}
	case schema.TBool:
		ops = []string{"="}
	case schema.TJSON:
		ops = []string{"contains"}
	}
	return append(ops, "empty", "not-empty")
}

func hasOp(t schema.Type, op string) bool {
	for _, o := range OpsFor(t) {
		if o == op {
			return true
		}
	}
	return false
}

func fieldType(s schema.Schema, path string) schema.Type {
	if f, ok := s.Field(path); ok && f.Type != "" {
		return f.Type
	}
	return schema.TString
}

// target is a filter operand parsed for its field type.
type target struct {
	i int64
	f float64
	b bool
	t time.Time
	s string
}

// parseOperand parses one filter value for type t.
func parseOperand(t schema.Type, v string) (target, error) {
	var out target
	switch t {
	case schema.TInt:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return out, fmt.Errorf("%q is not an integer", v)
		}
		out.i, out.f = n, float64(n)
	case schema.TFloat:
		n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return out, fmt.Errorf("%q is not a number", v)
		}
		out.f = n
	case schema.TDuration:
		n, ok := schema.ParseDuration(v)
		if !ok {
			return out, fmt.Errorf("%q is not a duration", v)
		}
		out.f = n
	case schema.TTimestamp:
		tm, err := parseTime(v)
		if err != nil {
			return out, fmt.Errorf("%q is not a timestamp", v)
		}
		out.t = tm
	case schema.TBool:
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return out, fmt.Errorf("%q is not a boolean", v)
		}
		out.b = b
	default:
		out.s = v
	}
	return out, nil
}

// parseTime accepts RFC 3339 or "2006-01-02 15:04:05[.000]" in local time.
func parseTime(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t, nil
	}
	return time.ParseInLocation("2006-01-02 15:04:05", v, time.Local)
}

// Validate rejects an operator that does not fit the field's type, a value
// that does not parse for it, and an unparseable range.
func (q Query) Validate(s schema.Schema) error {
	for _, f := range q.Filters {
		t := fieldType(s, f.Field)
		if !hasOp(t, f.Op) {
			return fmt.Errorf("operator %q is not valid for %s field %q", f.Op, t, f.Field)
		}
		switch f.Op {
		case "empty", "not-empty":
			if len(f.Values) != 0 {
				return fmt.Errorf("operator %q takes no value", f.Op)
			}
			continue
		case "in", "not-in":
			if len(f.Values) == 0 {
				return fmt.Errorf("operator %q needs at least one value", f.Op)
			}
		default:
			if len(f.Values) != 1 {
				return fmt.Errorf("operator %q on %q needs exactly one value", f.Op, f.Field)
			}
		}
		for _, v := range f.Values {
			if _, err := parseOperand(t, v); err != nil {
				return fmt.Errorf("field %q: %w", f.Field, err)
			}
		}
	}
	if _, _, err := q.Range.resolve(time.Now(), time.Time{}); err != nil {
		return err
	}
	return nil
}

// resolve turns the range into bounds. A zero from is unbounded; a zero to is
// unbounded above (the window ends "now", and records never arrive from the
// future on purpose, so a clock-skewed one is kept rather than hidden).
func (r Range) resolve(now, runStart time.Time) (from, to time.Time, err error) {
	switch f := strings.TrimSpace(r.From); f {
	case "", "all":
	case "this-run":
		from = runStart
	default:
		if d, derr := time.ParseDuration(f); derr == nil {
			from = now.Add(-d)
		} else if t, terr := time.Parse(time.RFC3339Nano, f); terr == nil {
			from = t
		} else {
			return from, to, fmt.Errorf("range from %q: want this-run, all, a duration or RFC 3339", r.From)
		}
	}
	switch t := strings.TrimSpace(r.To); t {
	case "", "now":
	default:
		tm, terr := time.Parse(time.RFC3339Nano, t)
		if terr != nil {
			return from, to, fmt.Errorf("range to %q: want now or RFC 3339", r.To)
		}
		to = tm
	}
	return from, to, nil
}
