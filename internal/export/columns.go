package export

import (
	"strings"

	"github.com/jimbeveridge/loghorn/internal/query"
	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/schema"
)

// ColumnDesc describes one column of an exported document: its name (field
// path or metric name), its DuckDB type in the SQL table model, its unit,
// whether this result holds a null in it, how many values were exported as
// null because they did not fit the type, and the original OpenTelemetry
// attribute key when loghorn's path differs from it.
type ColumnDesc struct {
	Name       string `json:"name" yaml:"name"`
	Type       string `json:"type" yaml:"type"`
	Unit       string `json:"unit,omitempty" yaml:"unit,omitempty"`
	Nullable   bool   `json:"nullable" yaml:"nullable"`
	Mismatched int    `json:"mismatched,omitempty" yaml:"mismatched,omitempty"`
	OTelName   string `json:"otel_name,omitempty" yaml:"otel_name,omitempty"`
}

// Columns describes the columns of res's data in a document, in order.
// Samples and Table: the result's columns. Graph: time, the group-by fields,
// count, then the plot metric (the series values). Heatmap: time, band, lo,
// hi and count (the fields of a sparse cell).
func Columns(res *query.Result, s schema.Schema) []ColumnDesc {
	switch res.View {
	case "graph":
		return graphColumns(res.Graph, s)
	case "heatmap":
		return heatmapColumns(res.Heatmap)
	}
	cols := make([]ColumnDesc, len(res.Columns))
	for i, c := range res.Columns {
		d := fieldDesc(c.Path, c.Type, c.Unit, s)
		if res.View == "table" && !c.Group {
			d = metricDesc(c)
		}
		for _, r := range res.Rows {
			if i >= len(r.Cells) {
				d.Nullable = true
				continue
			}
			if r.Cells[i].Bad {
				d.Mismatched++
			}
			if cellValue(r.Cells[i], c.Type) == nil {
				d.Nullable = true
			}
		}
		cols[i] = d
	}
	return cols
}

// fieldDesc describes a stored field: its SQL type, the unit (ms for a
// duration; a timestamp's epoch unit says how it was read, not what is
// exported, so it is left out) and its OTel attribute key.
func fieldDesc(path string, t schema.Type, unit string, s schema.Schema) ColumnDesc {
	typ, u := schema.SQLType(t)
	if u == "" && t != schema.TTimestamp {
		u = unit
	}
	d := ColumnDesc{Name: path, Type: typ, Unit: u}
	if k, ok := record.OTelName(s.Source, path); ok {
		d.OTelName = k
	}
	return d
}

// metricDesc describes a metric column (count, op(field), error_rate) by its
// SQL result type: count is BIGINT; avg, a percentile and error_rate are
// DOUBLE; sum, min and max keep the field's type.
func metricDesc(c query.Column) ColumnDesc {
	op, _, _ := strings.Cut(c.Path, "(")
	switch op {
	case "count":
		return ColumnDesc{Name: c.Path, Type: "BIGINT"}
	case "error_rate":
		return ColumnDesc{Name: c.Path, Type: "DOUBLE"}
	}
	typ, unit := schema.SQLType(c.Type)
	if op == "avg" || len(op) > 1 && op[0] == 'p' && op[1] >= '0' && op[1] <= '9' {
		typ = "DOUBLE"
	}
	return ColumnDesc{Name: c.Path, Type: typ, Unit: unit}
}

func graphColumns(g *query.Graph, s schema.Schema) []ColumnDesc {
	cols := []ColumnDesc{{Name: "time", Type: "TIMESTAMPTZ"}}
	if g == nil {
		return append(cols, ColumnDesc{Name: "count", Type: "BIGINT"})
	}
	for i, p := range g.GroupBy {
		f := groupField(s, p)
		d := fieldDesc(p, f.Type, f.Unit, s)
		for _, sr := range g.Series {
			if !sr.Other && i < len(sr.Group) && groupValue(sr.Group[i], f) == nil {
				d.Nullable = true
			}
		}
		cols = append(cols, d)
	}
	cols = append(cols, ColumnDesc{Name: "count", Type: "BIGINT"})
	if g.Metric.Path == "count" || g.Metric.Path == "" {
		return cols
	}
	m := metricDesc(g.Metric)
	for _, sr := range g.Series {
		for _, v := range sr.Values {
			if v == nil {
				m.Nullable = true
			}
		}
	}
	return append(cols, m)
}

func heatmapColumns(h *query.Heatmap) []ColumnDesc {
	return []ColumnDesc{
		{Name: "time", Type: "TIMESTAMPTZ"},
		{Name: "band", Type: "VARCHAR"},
		{Name: "lo", Type: "DOUBLE", Unit: schema.UnitMs, Nullable: h != nil && len(h.Bands) > 0},
		{Name: "hi", Type: "DOUBLE", Unit: schema.UnitMs},
		{Name: "count", Type: "BIGINT"},
	}
}

// groupField is the schema field of a group-by path; an unknown path is a
// string, as the query treats it.
func groupField(s schema.Schema, path string) schema.Field {
	f, ok := s.Field(path)
	if !ok || f.Type == "" {
		f.Path, f.Type = path, schema.TString
	}
	return f
}
