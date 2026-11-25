package query

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/schema"
)

// AggregateOps lists the metric operators; count is implicit and always first.
var AggregateOps = []string{"sum", "avg", "min", "max", "p50", "p90", "p95", "p99", "error_rate"}

// Buckets lists the valid time bucket widths for the graph view.
var Buckets = []string{"auto", "1s", "5s", "10s", "30s", "1m", "5m", "15m", "1h", "6h", "1d"}

// emptyLabel is the group label of an empty or bad group value.
const emptyLabel = "(empty)"

func isNumericType(t schema.Type) bool {
	return t == schema.TInt || t == schema.TFloat || t == schema.TDuration
}

func inList(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// view returns the query's view, "samples" when unset.
func (q Query) view() string {
	if q.View == "" {
		return "samples"
	}
	return q.View
}

// lookupField returns the schema field for path, or a string field.
func lookupField(s schema.Schema, path string) schema.Field {
	if f, ok := s.Field(path); ok {
		if f.Type == "" {
			f.Type = schema.TString
		}
		return f
	}
	return schema.Field{Path: path, Type: schema.TString, Visualizer: schema.DefaultVisualizer(path, schema.TString)}
}

// groupKey returns the display labels of r's group-by values and their joined
// key. An empty or bad value is labelled "(empty)" and flagged in empty; the
// key marks it with \x01 so it never merges with a real "(empty)" value.
func groupKey(r *record.Record, fields []schema.Field) (key string, labels []string, empty []bool) {
	labels = make([]string, len(fields))
	empty = make([]bool, len(fields))
	parts := make([]string, len(fields))
	for i, f := range fields {
		c := cell(r.Fields[f.Path], f)
		if c.Bad || c.V == nil {
			labels[i] = emptyLabel
			empty[i] = true
			parts[i] = "\x01"
		} else {
			labels[i] = fmt.Sprint(c.V)
			parts[i] = labels[i]
		}
	}
	return strings.Join(parts, "\x00"), labels, empty
}

// metricColumn describes the result column of aggregate a.
func metricColumn(a Aggregate, s schema.Schema) Column {
	if a.Op == "count" {
		return Column{Path: "count", Label: "count", Type: schema.TInt, Visualizer: "number"}
	}
	if a.Op == "error_rate" {
		return Column{Path: "error_rate", Label: "error rate", Type: schema.TFloat, Visualizer: "number"}
	}
	f := lookupField(s, a.Field)
	name := f.Label
	if name == "" {
		name = f.Path
	}
	typ := f.Type
	if a.Op == "avg" && typ == schema.TInt {
		typ = schema.TFloat
	}
	vis := f.Visualizer
	if (typ == schema.TInt || typ == schema.TFloat) && vis != "number" {
		vis = "number"
	}
	return Column{
		Path:       a.Op + "(" + a.Field + ")",
		Label:      a.Op + " " + name,
		Type:       typ,
		Unit:       f.Unit,
		Visualizer: vis,
	}
}

// percentile is the nearest-rank percentile of a sorted, non-empty slice.
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	i := int(math.Ceil(p/100*float64(n))) - 1
	if i < 0 {
		i = 0
	}
	if i >= n {
		i = n - 1
	}
	return sorted[i]
}

// metricField is the schema field aggregate a reads; error_rate reads none.
func metricField(s schema.Schema, a Aggregate) schema.Field {
	if a.Op == "error_rate" {
		return schema.Field{}
	}
	return lookupField(s, a.Field)
}

// isErrorSeverity reports whether v names a severity of ERROR or higher.
func isErrorSeverity(v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "ERROR", "CRITICAL", "ALERT", "EMERGENCY", "FATAL":
		return true
	}
	return false
}

// metricValue computes aggregate a over recs. It reports false when no record
// has a usable value, or the result is not finite.
func metricValue(recs []*record.Record, a Aggregate, f schema.Field) (float64, bool) {
	if a.Op == "error_rate" {
		if len(recs) == 0 {
			return 0, false
		}
		n := 0
		for _, r := range recs {
			if isErrorSeverity(r.Fields["severity"]) {
				n++
			}
		}
		return Round15(float64(n) / float64(len(recs))), true
	}
	vals := make([]float64, 0, len(recs))
	for _, r := range recs {
		v := schema.Convert(r.Fields[f.Path], f)
		if v.Bad || v.Empty {
			continue
		}
		x := v.Num
		if f.Type == schema.TInt {
			x = float64(v.Int)
		}
		if math.IsNaN(x) || math.IsInf(x, 0) {
			continue
		}
		vals = append(vals, x)
	}
	if len(vals) == 0 {
		return 0, false
	}
	var out float64
	switch a.Op {
	case "sum", "avg":
		for _, x := range vals {
			out += x
		}
		if a.Op == "avg" {
			out /= float64(len(vals))
		}
		// A sum of ints stays exact (metricCell rounds it to an int64).
		if a.Op == "avg" || f.Type != schema.TInt {
			out = Round15(out)
		}
	case "min":
		out = vals[0]
		for _, x := range vals {
			out = math.Min(out, x)
		}
	case "max":
		out = vals[0]
		for _, x := range vals {
			out = math.Max(out, x)
		}
	case "p50", "p90", "p95", "p99":
		sort.Float64s(vals)
		p, _ := strconv.ParseFloat(a.Op[1:], 64)
		out = percentile(vals, p)
	default:
		return 0, false
	}
	if math.IsNaN(out) || math.IsInf(out, 0) {
		return 0, false
	}
	return out, true
}

// Round15 rounds v to 15 significant digits, the digits a float64 holds
// reliably, so arithmetic noise (0.30000000000000004) goes. NaN and ±Inf
// pass through.
func Round15(v float64) float64 {
	if v == 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return v
	}
	r, err := strconv.ParseFloat(strconv.FormatFloat(v, 'g', 15, 64), 64)
	if err != nil {
		return v
	}
	return r
}

// metricCell encodes a metric value for a column of type typ.
func metricCell(v float64, ok bool, typ schema.Type) Cell {
	if !ok {
		return Cell{}
	}
	if typ == schema.TInt {
		if math.Abs(v) >= 1<<62 {
			return Cell{} // beyond what an int64 holds
		}
		n := int64(math.Round(v))
		if n > maxExactInt || n < -maxExactInt {
			return Cell{V: strconv.FormatInt(n, 10)}
		}
		return Cell{V: n}
	}
	return Cell{V: v}
}

type group struct {
	key    string
	labels []string
	empty  []bool
	recs   []*record.Record
}

// runTable groups matched records and fills res with one row per group.
func runTable(matched []*record.Record, s schema.Schema, q Query, res *Result) {
	gfields := make([]schema.Field, len(q.GroupBy))
	for i, p := range q.GroupBy {
		gfields[i] = lookupField(s, p)
	}
	var groups []*group
	if len(gfields) == 0 {
		groups = []*group{{recs: matched}}
	} else {
		byKey := map[string]*group{}
		for _, r := range matched {
			key, labels, empty := groupKey(r, gfields)
			g := byKey[key]
			if g == nil {
				g = &group{key: key, labels: labels, empty: empty}
				byKey[key] = g
				groups = append(groups, g)
			}
			g.recs = append(g.recs, r)
		}
		rankGroups(groups, func(g *group) ([]*record.Record, string) { return g.recs, g.key }, q.Order, s)
	}
	res.Groups = len(groups)

	limit := q.Top
	if limit <= 0 {
		limit = q.Limit
	}
	if limit <= 0 {
		limit = DefaultTop
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	if len(groups) > limit {
		groups = groups[:limit]
		res.Truncated = true
	}

	for _, f := range gfields {
		label := f.Label
		if label == "" {
			label = f.Path
		}
		res.Columns = append(res.Columns, Column{Path: f.Path, Label: label, Type: f.Type, Unit: f.Unit, Visualizer: f.Visualizer, Group: true})
	}
	res.Columns = append(res.Columns, metricColumn(Aggregate{Op: "count"}, s))
	for _, a := range q.Aggregates {
		res.Columns = append(res.Columns, metricColumn(a, s))
	}

	res.Rows = make([]Row, 0, len(groups))
	for _, g := range groups {
		cells := make([]Cell, 0, len(res.Columns))
		for i, f := range gfields {
			if g.empty[i] {
				cells = append(cells, Cell{})
			} else {
				cells = append(cells, cell(g.recs[0].Fields[f.Path], f))
			}
		}
		cells = append(cells, Cell{V: int64(len(g.recs))})
		for _, a := range q.Aggregates {
			col := metricColumn(a, s)
			v, ok := metricValue(g.recs, a, metricField(s, a))
			cells = append(cells, metricCell(v, ok, col.Type))
		}
		res.Rows = append(res.Rows, Row{Cells: cells})
	}
}

// rankStat is what ranking reads of a group: its record count, its order
// metric (ok false when it has none) and its key.
type rankStat struct {
	n   int
	v   float64
	ok  bool
	key string
}

// rankGroups orders groups by q.Order (count, descending, when nil). A group
// with no value for the metric sorts last in either direction; ties fall back
// to count descending, then key.
func rankGroups[G any](groups []G, of func(G) ([]*record.Record, string), o *Order, s schema.Schema) {
	by := Order{Op: "count"}
	if o != nil {
		by = *o
	}
	agg := Aggregate{Op: by.Op, Field: by.Field}
	var f schema.Field
	if by.Op != "count" && by.Op != "error_rate" {
		f = lookupField(s, by.Field)
	}
	rankStats(groups, func(g G) rankStat {
		recs, key := of(g)
		r := rankStat{n: len(recs), key: key}
		if by.Op == "count" {
			r.v, r.ok = float64(len(recs)), true
		} else {
			r.v, r.ok = metricValue(recs, agg, f)
			r.ok = r.ok && !math.IsNaN(r.v)
		}
		return r
	}, by.Asc)
}

// rankStats orders groups by their stats, as rankGroups describes.
func rankStats[G any](groups []G, stat func(G) rankStat, asc bool) {
	type ranked struct {
		g G
		rankStat
	}
	rs := make([]ranked, len(groups))
	for i, g := range groups {
		rs[i] = ranked{g, stat(g)}
	}
	sort.SliceStable(rs, func(a, b int) bool {
		x, y := rs[a], rs[b]
		if x.ok != y.ok {
			return x.ok
		}
		if x.ok && x.v != y.v {
			if asc {
				return x.v < y.v
			}
			return x.v > y.v
		}
		if x.n != y.n {
			return x.n > y.n
		}
		return x.key < y.key
	})
	for i, r := range rs {
		groups[i] = r.g
	}
}
