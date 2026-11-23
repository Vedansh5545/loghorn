package export

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/toon-format/toon-go"
	"gopkg.in/yaml.v3"

	"github.com/jimbeveridge/loghorn/internal/query"
	"github.com/jimbeveridge/loghorn/internal/schema"
)

// A document is built from ordered values, so each encoder keeps the key
// order: nil, string, bool, int64, float64 (NaN and ±Inf included),
// json.Number, object and []any.
type object []member

type member struct {
	key string
	val any
}

func (o *object) add(key string, val any) { *o = append(*o, member{key, val}) }

// writeDoc writes res as a JSON, YAML or TOON document: view, columns, the
// counts and the resolved range, then the view's data.
func writeDoc(w io.Writer, f Format, res *query.Result, s schema.Schema) error {
	doc := buildDoc(res, s)
	switch f {
	case YAML:
		enc := yaml.NewEncoder(w)
		enc.SetIndent(2)
		if err := enc.Encode(yamlNode(doc)); err != nil {
			return err
		}
		return enc.Close()
	case TOON:
		b, err := toon.Marshal(toonValue(doc))
		if err != nil {
			return err
		}
		if len(b) == 0 || b[len(b)-1] != '\n' {
			b = append(b, '\n')
		}
		_, err = w.Write(b)
		return err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, appendJSON(nil, doc), "", "  "); err != nil {
		return err
	}
	out.WriteByte('\n')
	_, err := out.WriteTo(w)
	return err
}

func buildDoc(res *query.Result, s schema.Schema) object {
	var doc object
	doc.add("view", viewName(res.View))
	cols := []any{}
	for _, c := range Columns(res, s) {
		cols = append(cols, descObject(c))
	}
	doc.add("columns", cols)
	doc.add("matched", int64(res.Matched))
	doc.add("streamTotal", int64(res.StreamTotal))
	doc.add("truncated", res.Truncated)
	doc.add("from", timeValue(res.From))
	doc.add("to", timeValue(res.To))
	switch res.View {
	case "table":
		doc.add("groups", tableGroups(res))
	case "graph":
		graphDoc(&doc, res.Graph, s)
	case "heatmap":
		heatmapDoc(&doc, res.Heatmap)
	default:
		doc.add("rows", sampleRows(res))
	}
	return doc
}

func viewName(v string) string {
	if v == "" {
		return "samples"
	}
	return v
}

func timeValue(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return timeText(t)
}

func descObject(c ColumnDesc) object {
	o := object{{"name", c.Name}, {"type", c.Type}}
	if c.Unit != "" {
		o.add("unit", c.Unit)
	}
	o.add("nullable", c.Nullable)
	if c.Mismatched > 0 {
		o.add("mismatched", int64(c.Mismatched))
	}
	if c.OTelName != "" {
		o.add("otel_name", c.OTelName)
	}
	return o
}

// docValue turns an export value into a document value: compact JSON text
// becomes nested structure, keys in their original order.
func docValue(v value) any {
	raw, ok := v.(json.RawMessage)
	if !ok {
		return v
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	out, err := readJSON(dec)
	if err != nil {
		return string(raw)
	}
	return out
}

func readJSON(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return tok, nil // nil, bool, string or json.Number
	}
	switch d {
	case '{':
		o := object{}
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				return nil, err
			}
			v, err := readJSON(dec)
			if err != nil {
				return nil, err
			}
			o.add(k.(string), v)
		}
		_, err = dec.Token()
		return o, err
	case '[':
		a := []any{}
		for dec.More() {
			v, err := readJSON(dec)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		_, err = dec.Token()
		return a, err
	}
	return nil, nil
}

func sampleRows(res *query.Result) []any {
	t := flatten(res)
	rows := make([]any, 0, len(t.rows))
	for _, r := range t.rows {
		o := make(object, 0, len(t.header))
		for i, h := range t.header {
			var v value
			if i < len(r) {
				v = r[i]
			}
			o.add(h, docValue(v))
		}
		rows = append(rows, o)
	}
	return rows
}

// nestItem is one innermost element of a nested array: its value at each
// group-by level, then its own members.
type nestItem struct {
	keys []any
	tail object
}

// nest builds the array for group-by level, merging items with an equal key
// at every level but the last. A parent holds its key and the next level's
// array under child; parents come in order of their first item.
func nest(paths []string, child string, items []nestItem, level int) []any {
	out := []any{}
	if len(paths) == 0 {
		for _, it := range items {
			out = append(out, it.tail)
		}
		return out
	}
	if level == len(paths)-1 {
		for _, it := range items {
			o := object{{paths[level], it.keys[level]}}
			out = append(out, append(o, it.tail...))
		}
		return out
	}
	var order []string
	byKey := map[string][]nestItem{}
	first := map[string]any{}
	for _, it := range items {
		k := keyString(it.keys[level])
		if _, seen := byKey[k]; !seen {
			order = append(order, k)
			first[k] = it.keys[level]
		}
		byKey[k] = append(byKey[k], it)
	}
	for _, k := range order {
		out = append(out, object{{paths[level], first[k]}, {child, nest(paths, child, byKey[k], level+1)}})
	}
	return out
}

// keyString identifies a group value, its Go type included so the string
// "200" and the number 200 stay apart.
func keyString(v any) string {
	if v == nil {
		return "\x00"
	}
	b := appendJSON(nil, v)
	switch v.(type) {
	case string:
		return "s" + string(b)
	case bool:
		return "b" + string(b)
	}
	return "n" + string(b)
}

func tableGroups(res *query.Result) []any {
	t := flatten(res)
	var paths []string
	ng := 0
	for _, c := range res.Columns {
		if c.Group {
			paths = append(paths, c.Path)
			ng++
		}
	}
	items := make([]nestItem, 0, len(t.rows))
	for _, r := range t.rows {
		it := nestItem{keys: make([]any, ng)}
		for i, h := range t.header {
			var v value
			if i < len(r) {
				v = r[i]
			}
			if i < ng {
				it.keys[i] = docValue(v)
			} else {
				it.tail.add(h, docValue(v))
			}
		}
		items = append(items, it)
	}
	return nest(paths, "groups", items, 0)
}

// groupValue is a graph group label as a typed value: null for the empty
// group, a number or bool where the field's type says so, else the label.
func groupValue(label string, f schema.Field) any {
	if label == "(empty)" {
		return nil
	}
	switch f.Type {
	case schema.TInt, schema.TFloat:
		if isJSONNumber(label) {
			return json.Number(label)
		}
	case schema.TDuration:
		if v, err := strconv.ParseFloat(label, 64); err == nil {
			return durationMs(v)
		}
	case schema.TBool:
		if b, err := strconv.ParseBool(label); err == nil {
			return b
		}
	}
	return label
}

func graphDoc(doc *object, g *query.Graph, s schema.Schema) {
	if g == nil {
		g = &query.Graph{}
	}
	doc.add("bucket", g.Bucket)
	starts := make([]any, len(g.Starts))
	for i, t := range g.Starts {
		starts[i] = timeText(t)
	}
	doc.add("starts", starts)
	values := func(sr query.Series) []any {
		vs := make([]any, len(g.Starts))
		for i := range vs {
			if i < len(sr.Values) && sr.Values[i] != nil {
				vs[i] = metricNum(*sr.Values[i], g.Metric.Type)
			}
		}
		return vs
	}
	fields := make([]schema.Field, len(g.GroupBy))
	for i, p := range g.GroupBy {
		fields[i] = groupField(s, p)
	}
	var items []nestItem
	var others []any
	for _, sr := range g.Series {
		tail := object{{"count", int64(sr.Count)}, {"values", values(sr)}}
		if sr.Other {
			others = append(others, append(object{{"other", true}}, tail...))
			continue
		}
		it := nestItem{keys: make([]any, len(fields)), tail: tail}
		for i, f := range fields {
			if i < len(sr.Group) {
				it.keys[i] = groupValue(sr.Group[i], f)
			}
		}
		items = append(items, it)
	}
	doc.add("series", append(nest(g.GroupBy, "series", items, 0), others...))
}

func heatmapDoc(doc *object, h *query.Heatmap) {
	if h == nil {
		h = &query.Heatmap{}
	}
	doc.add("bucket", h.Bucket)
	bands := make([]any, len(h.Bands))
	names := make([]string, len(h.Bands))
	for j, b := range h.Bands {
		names[j] = BandName(b, j == 0)
		o := object{{"band", names[j]}}
		if j > 0 {
			o.add("lo", b.Lo)
		}
		o.add("hi", b.Hi)
		bands[j] = o
	}
	doc.add("bands", bands)
	doc.add("max", int64(h.Max))
	doc.add("total", int64(h.Total))
	doc.add("skipped", int64(h.Skipped))
	buckets := []any{}
	for i, st := range h.Starts {
		if i >= len(h.Counts) {
			break
		}
		cells := []any{}
		for j := range h.Bands {
			if j < len(h.Counts[i]) && h.Counts[i][j] > 0 {
				cells = append(cells, object{{"band", names[j]}, {"count", int64(h.Counts[i][j])}})
			}
		}
		if len(cells) > 0 {
			buckets = append(buckets, object{{"time", timeText(st)}, {"cells", cells}})
		}
	}
	doc.add("buckets", buckets)
}

// appendJSON appends v as compact JSON: NaN and ±Inf as strings, HTML
// characters unescaped.
func appendJSON(b []byte, v any) []byte {
	switch x := v.(type) {
	case nil:
		return append(b, "null"...)
	case string:
		return append(b, jsonText(x)...)
	case bool:
		return strconv.AppendBool(b, x)
	case int64:
		return strconv.AppendInt(b, x, 10)
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return append(b, jsonText(floatText(x))...)
		}
		return append(b, num(x)...)
	case json.Number:
		return append(b, x...)
	case object:
		b = append(b, '{')
		for i, m := range x {
			if i > 0 {
				b = append(b, ',')
			}
			b = append(b, jsonText(m.key)...)
			b = append(b, ':')
			b = appendJSON(b, m.val)
		}
		return append(b, '}')
	case []any:
		b = append(b, '[')
		for i, e := range x {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendJSON(b, e)
		}
		return append(b, ']')
	}
	return append(b, "null"...)
}

// yamlNode builds the YAML tree with explicit tags: a string tagged !!str stays
// a string even when it reads as a number, bool or null ("200", "true",
// "null"), and a whole float is tagged !!int, since yaml.v3 otherwise prints
// "!!float 3".
func yamlNode(v any) *yaml.Node {
	scalar := func(tag, val string) *yaml.Node {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: val}
	}
	switch x := v.(type) {
	case nil:
		return scalar("!!null", "null")
	case string:
		return scalar("!!str", x)
	case bool:
		return scalar("!!bool", strconv.FormatBool(x))
	case int64:
		return scalar("!!int", strconv.FormatInt(x, 10))
	case float64:
		switch {
		case math.IsNaN(x):
			return scalar("!!float", ".nan")
		case math.IsInf(x, 1):
			return scalar("!!float", ".inf")
		case math.IsInf(x, -1):
			return scalar("!!float", "-.inf")
		}
		// A whole number prints as an int, as in JSON, rather than as
		// "!!float 3".
		s := num(x)
		if strings.ContainsAny(s, ".eE") {
			return scalar("!!float", s)
		}
		return scalar("!!int", s)
	case json.Number:
		if _, err := strconv.ParseInt(string(x), 10, 64); err == nil {
			return scalar("!!int", string(x))
		}
		return scalar("!!float", string(x))
	case object:
		n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		for _, m := range x {
			n.Content = append(n.Content, scalar("!!str", m.key), yamlNode(m.val))
		}
		return n
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, e := range x {
			n.Content = append(n.Content, yamlNode(e))
		}
		return n
	}
	return scalar("!!null", "null")
}

// toonValue converts a document value for toon-go: object becomes its ordered
// toon.Object, NaN and ±Inf the strings JSON uses. toon-go keeps key order
// only through toon.Object/toon.Field (it sorts Go maps), and turns a
// json.Number into a float64 with no size check, so an integer beyond 2^53 is
// passed as its digits (TOON then quotes it) rather than silently rounded.
func toonValue(v any) any {
	switch x := v.(type) {
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return floatText(x)
		}
	case json.Number:
		// toon-go parses a number as float64; an int it cannot hold
		// exactly stays its digits, as toon-go does for int64.
		if n, err := strconv.ParseInt(string(x), 10, 64); err == nil && (n > 1<<53 || n < -(1<<53)) {
			return string(x)
		}
	case object:
		fs := make([]toon.Field, len(x))
		for i, m := range x {
			fs[i] = toon.Field{Key: m.key, Value: toonValue(m.val)}
		}
		return toon.NewObject(fs...)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = toonValue(e)
		}
		return out
	}
	return v
}
