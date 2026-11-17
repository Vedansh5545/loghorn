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
var AggregateOps = []string{"sum", "avg", "min", "max", "p50", "p95", "p99"}

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
// key. An empty or bad value is labelled "(empty)".
func groupKey(r *record.Record, fields []schema.Field) (key string, labels []string) {
	labels = make([]string, len(fields))
	for i, f := range fields {
		c := cell(r.Fields[f.Path], f)
		if c.Bad || c.V == nil {
			labels[i] = emptyLabel
		} else {
			labels[i] = fmt.Sprint(c.V)
		}
	}
	return strings.Join(labels, "\x00"), labels
}

// metricColumn describes the result column of aggregate a.
func metricColumn(a Aggregate, s schema.Schema) Column {
	if a.Op == "count" {
		return Column{Path: "count", Label: "count", Type: schema.TInt, Visualizer: "number"}
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

// metricValue computes aggregate a over recs. It reports false when no record
// has a usable value, or the result is not finite.
func metricValue(recs []*record.Record, a Aggregate, f schema.Field) (float64, bool) {
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
	case "p50", "p95", "p99":
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
			key, labels := groupKey(r, gfields)
			g := byKey[key]
			if g == nil {
				g = &group{key: key, labels: labels}
				byKey[key] = g
				groups = append(groups, g)
			}
			g.recs = append(g.recs, r)
		}
		sort.SliceStable(groups, func(a, b int) bool {
			if len(groups[a].recs) != len(groups[b].recs) {
				return len(groups[a].recs) > len(groups[b].recs)
			}
			return groups[a].key < groups[b].key
		})
	}
	res.Groups = len(groups)

	limit := q.Limit
	if limit <= 0 {
		limit = DefaultLimit
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
			if g.labels[i] == emptyLabel {
				cells = append(cells, Cell{})
			} else {
				cells = append(cells, cell(g.recs[0].Fields[f.Path], f))
			}
		}
		cells = append(cells, Cell{V: int64(len(g.recs))})
		for _, a := range q.Aggregates {
			col := metricColumn(a, s)
			v, ok := metricValue(g.recs, a, lookupField(s, a.Field))
			cells = append(cells, metricCell(v, ok, col.Type))
		}
		res.Rows = append(res.Rows, Row{Cells: cells})
	}
}
