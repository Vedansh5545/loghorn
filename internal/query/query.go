// Package query runs typed filters, a time range, a text search and a sort
// over the records of one stream.
package query

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/schema"
)

// Row limits.
const (
	DefaultLimit = 100
	MaxLimit     = 10000
)

// Group limits (Top): how many groups the table shows and the graph draws
// before the rest fold into "other".
const (
	DefaultTop  = 10
	MaxGraphTop = 50
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

// Aggregate is one metric: an operator over a numeric field. Op "count"
// needs no field (used by Plot).
type Aggregate struct {
	Op    string `json:"op" toml:"op"`
	Field string `json:"field,omitempty" toml:"field,omitempty"`
}

// Query is the saved/serialised query model.
type Query struct {
	Stream string `json:"stream" toml:"stream"`
	// Source is record.SourceLog or record.SourceOtel; "" means log.
	Source string `json:"source,omitempty" toml:"source,omitempty"`
	// Service restricts a run to one service's records; "" and "*" mean
	// every service. It is not saved with a favorite.
	Service string   `json:"service,omitempty" toml:"-"`
	Range   Range    `json:"range" toml:"range"`
	Filters []Filter `json:"filters" toml:"filters"`
	Search  string   `json:"search" toml:"search"`
	Columns []string `json:"columns" toml:"columns"`
	Sort    Sort     `json:"sort" toml:"sort"`
	Limit   int      `json:"limit" toml:"limit"`
	// Offset skips that many sorted rows of a samples query, for the page's
	// "load more". It is not saved with a favorite.
	Offset int `json:"offset,omitempty" toml:"-"`

	// Explore views. View is samples (default), table or graph. GroupBy and
	// Aggregates drive table and graph; Bucket and Plot drive graph.
	GroupBy    []string    `json:"groupBy,omitempty" toml:"groupBy,omitempty"`
	Aggregates []Aggregate `json:"aggregates,omitempty" toml:"aggregates,omitempty"`
	Bucket     string      `json:"bucket,omitempty" toml:"bucket,omitempty"`
	View       string      `json:"view,omitempty" toml:"view,omitempty"`
	Plot       *Aggregate  `json:"plot,omitempty" toml:"plot,omitempty"`
	Order      *Order      `json:"order,omitempty" toml:"order,omitempty"`
	// Chart is how the page draws a graph: "line" (the default) or
	// "stacked". The server only checks it, so favorites keep it.
	Chart string `json:"chart,omitempty" toml:"chart,omitempty"`
	// Top is how many groups the table and graph keep, ranked by Order;
	// 0 means DefaultTop. The table falls back to Limit when Top is 0 and
	// Limit is set. The graph keeps at most MaxGraphTop.
	Top int `json:"top,omitempty" toml:"top,omitempty,omitzero"`
}

// Charts are the graph styles the page can draw.
var Charts = []string{"line", "stacked"}

// Order ranks the groups of the table and graph by a metric: count or one of
// the aggregates. Groups with no value for the metric always come last.
type Order struct {
	Op    string `json:"op" toml:"op"`
	Field string `json:"field,omitempty" toml:"field,omitempty"`
	Asc   bool   `json:"asc,omitempty" toml:"asc,omitempty"`
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

// Validate rejects an unknown source, an operator that does not fit the
// field's type, a value that does not parse for it, and an unparseable range.
func (q Query) Validate(s schema.Schema) error {
	if _, ok := record.ParseSource(q.Source); !ok {
		return fmt.Errorf("unknown source %q", q.Source)
	}
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
	if _, _, err := q.Range.Resolve(time.Now(), time.Time{}); err != nil {
		return err
	}
	return q.validateExplore(s)
}

func (q Query) validateExplore(s schema.Schema) error {
	switch q.View {
	case "", "samples", "table", "graph":
	default:
		return fmt.Errorf("view %q: want samples, table or graph", q.View)
	}
	for _, g := range q.GroupBy {
		if g == "" {
			return fmt.Errorf("group-by field is empty")
		}
	}
	for _, a := range q.Aggregates {
		if !inList(AggregateOps, a.Op) {
			return fmt.Errorf("aggregate operator %q: want one of %s", a.Op, strings.Join(AggregateOps, ", "))
		}
		if a.Field == "" {
			return fmt.Errorf("aggregate %q needs a field", a.Op)
		}
		if t := fieldType(s, a.Field); !isNumericType(t) {
			return fmt.Errorf("aggregate %q needs a numeric field; %q is %s", a.Op, a.Field, t)
		}
	}
	if q.Offset < 0 {
		return fmt.Errorf("offset %d: want 0 or more", q.Offset)
	}
	if q.Top < 0 || q.Top > MaxLimit {
		return fmt.Errorf("top %d: want 1 to %d", q.Top, MaxLimit)
	}
	if q.Chart != "" && !inList(Charts, q.Chart) {
		return fmt.Errorf("chart %q: want one of %s", q.Chart, strings.Join(Charts, ", "))
	}
	if q.Bucket != "" && !inList(Buckets, q.Bucket) {
		return fmt.Errorf("bucket %q: want one of %s", q.Bucket, strings.Join(Buckets, ", "))
	}
	if p := q.Plot; p != nil && !(p.Op == "count" && p.Field == "") {
		found := false
		for _, a := range q.Aggregates {
			if a == *p {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("plot %s(%s) must be count or one of the aggregates", p.Op, p.Field)
		}
	}
	if o := q.Order; o != nil && !(o.Op == "count" && o.Field == "") {
		found := false
		for _, a := range q.Aggregates {
			if a == (Aggregate{Op: o.Op, Field: o.Field}) {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("order %s(%s) must be count or one of the aggregates", o.Op, o.Field)
		}
	}
	return nil
}

// agoUnits are the units ParseAgo accepts, longest first so "ms" wins over "m".
var agoUnits = []struct {
	name string
	d    time.Duration
}{
	{"ms", time.Millisecond}, {"s", time.Second}, {"m", time.Minute},
	{"h", time.Hour}, {"d", 24 * time.Hour}, {"w", 7 * 24 * time.Hour},
}

// ParseAgo reads a positive relative time such as "24m", "3.7h", "1d" or
// "1d12h": one or more decimal numbers, each with a unit of ms, s, m, h, d
// (24 h) or w (7 d), case-insensitive.
func ParseAgo(s string) (time.Duration, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0, false
	}
	var total float64
	for s != "" {
		i := 0
		dot := false
		for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.' && !dot) {
			dot = dot || s[i] == '.'
			i++
		}
		n, err := strconv.ParseFloat(s[:i], 64)
		if i == 0 || err != nil {
			return 0, false
		}
		s = s[i:]
		unit := time.Duration(0)
		for _, u := range agoUnits {
			if strings.HasPrefix(s, u.name) {
				unit = u.d
				s = s[len(u.name):]
				break
			}
		}
		if unit == 0 {
			return 0, false
		}
		total += n * float64(unit)
	}
	if total <= 0 || total > float64(math.MaxInt64) {
		return 0, false
	}
	return time.Duration(total), true
}

// Resolve turns the range into bounds. A zero from is unbounded; a zero to is
// unbounded above (the window ends "now", and records never arrive from the
// future on purpose, so a clock-skewed one is kept rather than hidden).
func (r Range) Resolve(now, runStart time.Time) (from, to time.Time, err error) {
	switch f := strings.TrimSpace(r.From); f {
	case "", "all":
	case "this-run":
		from = runStart
	default:
		if d, ok := ParseAgo(f); ok {
			from = now.Add(-d)
		} else if t, terr := time.Parse(time.RFC3339Nano, f); terr == nil {
			from = t
		} else {
			return from, to, fmt.Errorf("range from %q: want this-run, all, a relative time such as 1d or 3.7h, or RFC 3339", r.From)
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
