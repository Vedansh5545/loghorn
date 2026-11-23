package export

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jimbeveridge/loghorn/internal/query"
	"github.com/jimbeveridge/loghorn/internal/schema"
)

// A value in a flat table is one of: nil (null), string, int64, float64,
// bool, json.Number (a number kept as text, e.g. an int above 2^53) or
// json.RawMessage (compact JSON from a JSON-typed field).
type value = any

// table is the flat shape of a result: column names and rows of values.
type table struct {
	header []string
	rows   [][]value
}

func writeFlat(w io.Writer, f Format, res *query.Result) error {
	if f == JSONL && res.View == "heatmap" {
		return heatmapLines(w, res.Heatmap)
	}
	t := flatten(res)
	if len(t.header) == 0 {
		return nil
	}
	switch f {
	case CSV:
		return writeCSV(w, t)
	case TSV:
		return writeLines(w, t, tsvEscape, "\t", "", "")
	case MD:
		return writeMD(w, t)
	}
	return writeJSONL(w, t)
}

func flatten(res *query.Result) table {
	switch res.View {
	case "graph":
		return graphTable(res.Graph)
	case "heatmap":
		return heatmapTable(res.Heatmap)
	}
	t := table{header: make([]string, len(res.Columns))}
	for i, c := range res.Columns {
		t.header[i] = c.Path
	}
	for _, r := range res.Rows {
		row := make([]value, len(res.Columns))
		for i, c := range res.Columns {
			if i < len(r.Cells) {
				row[i] = cellValue(r.Cells[i], c.Type)
			}
		}
		t.rows = append(t.rows, row)
	}
	return t
}

// cellValue is the export value of a cell in a column of type typ. A bad
// cell (a value that does not fit the type) exports as null.
func cellValue(c query.Cell, typ schema.Type) value {
	if c.Bad || c.V == nil {
		return nil
	}
	switch v := c.V.(type) {
	case string:
		switch typ {
		case schema.TInt, schema.TFloat, schema.TDuration:
			// An int beyond 2^53 travels as its digits.
			if isJSONNumber(v) {
				return json.Number(v)
			}
		case schema.TJSON:
			var b bytes.Buffer
			if json.Compact(&b, []byte(v)) == nil {
				return json.RawMessage(b.Bytes())
			}
		}
		return v
	case int:
		return int64(v)
	case int64, float64, bool, json.Number:
		return v
	}
	b, err := json.Marshal(c.V)
	if err != nil {
		return nil
	}
	return json.RawMessage(b)
}

// isJSONNumber reports whether s is a JSON number (not NaN, Inf or hex).
func isJSONNumber(s string) bool {
	return s != "" && (s[0] == '-' || s[0] >= '0' && s[0] <= '9') && json.Valid([]byte(s))
}

func timeText(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// graphTable is the wide graph: time, then one column per series. With no
// group-by the single series is named by the plot metric.
func graphTable(g *query.Graph) table {
	t := table{header: []string{"time"}}
	if g == nil {
		return t
	}
	used := map[string]bool{"time": true}
	unique := func(name string) string {
		out := name
		for n := 2; used[out]; n++ {
			out = name + " (" + strconv.Itoa(n) + ")"
		}
		used[out] = true
		return out
	}
	metric := g.Metric.Path
	if metric == "" {
		metric = "count"
	}
	if len(g.Series) == 0 && g.Top == 0 {
		t.header = append(t.header, metric) // no group-by, no data
	}
	for _, s := range g.Series {
		name := s.Label
		switch {
		case s.Other:
			name = "other"
		case len(s.Group) == 0:
			name = metric
		}
		t.header = append(t.header, unique(name))
	}
	for i, st := range g.Starts {
		row := []value{timeText(st)}
		for _, s := range g.Series {
			var v value
			if i < len(s.Values) && s.Values[i] != nil {
				v = *s.Values[i]
			}
			row = append(row, v)
		}
		t.rows = append(t.rows, row)
	}
	return t
}

// heatmapTable is the wide heatmap: time, then one count column per band.
func heatmapTable(h *query.Heatmap) table {
	t := table{header: []string{"time"}}
	if h == nil {
		return t
	}
	for i, b := range h.Bands {
		t.header = append(t.header, BandName(b, i == 0))
	}
	for i, st := range h.Starts {
		row := []value{timeText(st)}
		for j := range h.Bands {
			n := 0
			if i < len(h.Counts) && j < len(h.Counts[i]) {
				n = h.Counts[i][j]
			}
			row = append(row, int64(n))
		}
		t.rows = append(t.rows, row)
	}
	return t
}

// heatmapLine is one non-empty heatmap cell in JSON Lines. Lo is omitted on
// the lowest band, which also holds zero and negative durations.
type heatmapLine struct {
	Time  string   `json:"time"`
	Band  string   `json:"band"`
	Lo    *float64 `json:"lo,omitempty"`
	Hi    float64  `json:"hi"`
	Count int      `json:"count"`
}

func heatmapLines(w io.Writer, h *query.Heatmap) error {
	if h == nil {
		return nil
	}
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	for i, st := range h.Starts {
		if i >= len(h.Counts) {
			break
		}
		for j, b := range h.Bands {
			if j >= len(h.Counts[i]) || h.Counts[i][j] == 0 {
				continue
			}
			line := heatmapLine{Time: timeText(st), Band: BandName(b, j == 0), Hi: b.Hi, Count: h.Counts[i][j]}
			if j > 0 {
				lo := b.Lo
				line.Lo = &lo
			}
			if err := enc.Encode(line); err != nil {
				return err
			}
		}
	}
	return bw.Flush()
}

// text is a value as a CSV/TSV/Markdown cell: null is empty, JSON is
// compact text, NaN and ±Inf are NaN, Infinity, -Infinity.
func text(v value) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return floatText(x)
	case bool:
		return strconv.FormatBool(x)
	case json.Number:
		return x.String()
	case json.RawMessage:
		return string(x)
	}
	return ""
}

func floatText(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "Infinity"
	case math.IsInf(v, -1):
		return "-Infinity"
	}
	return num(v)
}

func writeCSV(w io.Writer, t table) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(t.header); err != nil {
		return err
	}
	rec := make([]string, len(t.header))
	for _, row := range t.rows {
		for i := range rec {
			rec[i] = ""
			if i < len(row) {
				rec[i] = text(row[i])
			}
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

var tsvEscaper = strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`, "\r", `\r`)

func tsvEscape(s string) string { return tsvEscaper.Replace(s) }

var mdEscaper = strings.NewReplacer("|", `\|`, "\r\n", "<br>", "\n", "<br>", "\r", "<br>")

func mdEscape(s string) string { return mdEscaper.Replace(s) }

// writeLines writes the header and rows, each cell escaped, cells joined by
// sep, each line wrapped in open and close.
func writeLines(w io.Writer, t table, esc func(string) string, sep, open, close string) error {
	bw := bufio.NewWriter(w)
	line := func(cells []string) {
		bw.WriteString(open)
		for i, c := range cells {
			if i > 0 {
				bw.WriteString(sep)
			}
			bw.WriteString(esc(c))
		}
		bw.WriteString(close)
		bw.WriteByte('\n')
	}
	line(t.header)
	if open != "" { // Markdown: the separator row
		dashes := make([]string, len(t.header))
		for i := range dashes {
			dashes[i] = "---"
		}
		line(dashes)
	}
	cells := make([]string, len(t.header))
	for _, row := range t.rows {
		for i := range cells {
			cells[i] = ""
			if i < len(row) {
				cells[i] = text(row[i])
			}
		}
		line(cells)
	}
	return bw.Flush()
}

func writeMD(w io.Writer, t table) error {
	return writeLines(w, t, mdEscape, " | ", "| ", " |")
}

// writeJSONL writes one JSON object per row, keys in column order.
func writeJSONL(w io.Writer, t table) error {
	bw := bufio.NewWriter(w)
	keys := make([][]byte, len(t.header))
	for i, h := range t.header {
		keys[i] = jsonText(h)
	}
	for _, row := range t.rows {
		bw.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				bw.WriteByte(',')
			}
			bw.Write(k)
			bw.WriteByte(':')
			var v value
			if i < len(row) {
				v = row[i]
			}
			bw.Write(jsonValue(v))
		}
		bw.WriteString("}\n")
	}
	return bw.Flush()
}

// jsonValue encodes a value for JSON Lines: NaN and ±Inf as strings.
func jsonValue(v value) []byte {
	switch x := v.(type) {
	case nil:
		return []byte("null")
	case string:
		return jsonText(x)
	case int64:
		return strconv.AppendInt(nil, x, 10)
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return jsonText(floatText(x))
		}
		return []byte(num(x))
	case bool:
		return strconv.AppendBool(nil, x)
	case json.Number:
		return []byte(x.String())
	case json.RawMessage:
		return x
	}
	return []byte("null")
}

// jsonText encodes a string as JSON without HTML escaping.
func jsonText(s string) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}
