package query

import (
	"bytes"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/schema"
	"github.com/jimbeveridge/loghorn/internal/sqlcolor"
	"github.com/jimbeveridge/loghorn/internal/sqlfmt"
)

// Column describes one result column.
type Column struct {
	Path       string      `json:"path"`
	Label      string      `json:"label"`
	Type       schema.Type `json:"type"`
	Unit       string      `json:"unit"`
	Visualizer string      `json:"visualizer"`
	Group      bool        `json:"group,omitempty"` // a group-by column (table view)
}

// Cell is one value; V is nil for empty, the raw string with Bad set when the
// value does not fit the column's type.
type Cell struct {
	V   any  `json:"v"`
	Bad bool `json:"bad,omitempty"`
	// SQL colours a string in a column shown with the sql visualizer.
	SQL []sqlcolor.Token `json:"sql,omitempty"`
}

// Row is one matched record.
type Row struct {
	Seq           int       `json:"seq"`
	Time          time.Time `json:"time"`
	TimeInherited bool      `json:"timeInherited,omitempty"`
	Cells         []Cell    `json:"cells"`
}

// Result is the answer to a query. A zero From means unbounded.
type Result struct {
	View        string    `json:"view"`
	Groups      int       `json:"groups,omitempty"` // table: groups before the limit
	Columns     []Column  `json:"columns"`
	Rows        []Row     `json:"rows"`
	Graph       *Graph    `json:"graph,omitempty"`
	Matched     int       `json:"matched"`
	StreamTotal int       `json:"streamTotal"`
	Truncated   bool      `json:"truncated"`
	From        time.Time `json:"from"`
	To          time.Time `json:"to"`
}

// maxExactInt is the largest magnitude a browser number holds exactly.
const maxExactInt = 1 << 53

type compiled struct {
	field   string
	typ     schema.Type
	sfield  schema.Field
	op      string
	targets []target
}

func compile(s schema.Schema, q Query) []compiled {
	out := make([]compiled, 0, len(q.Filters))
	for _, f := range q.Filters {
		t := fieldType(s, f.Field)
		sf, ok := s.Field(f.Field)
		if !ok {
			sf = schema.Field{Path: f.Field, Type: t}
		}
		c := compiled{field: f.Field, typ: t, sfield: sf, op: f.Op}
		for _, v := range f.Values {
			tg, _ := parseOperand(t, v) // Validate has checked
			if t == schema.TString || t == schema.TJSON {
				if f.Op == "contains" {
					tg.s = strings.ToLower(v)
				}
			}
			c.targets = append(c.targets, tg)
		}
		out = append(out, c)
	}
	return out
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpTime(a, b time.Time) int {
	switch {
	case a.Before(b):
		return -1
	case a.After(b):
		return 1
	}
	return 0
}

func ordApplies(op string, c int) bool {
	switch op {
	case "=":
		return c == 0
	case "!=":
		return c != 0
	case "<":
		return c < 0
	case "<=":
		return c <= 0
	case ">":
		return c > 0
	case ">=":
		return c >= 0
	}
	return false
}

func (c compiled) match(v schema.Value) bool {
	if v.Bad {
		return c.op == "not-empty"
	}
	if v.Empty {
		return c.op == "empty" || c.op == "!=" || c.op == "not-in"
	}
	switch c.op {
	case "empty":
		return false
	case "not-empty":
		return true
	}
	t := c.targets[0]
	switch c.typ {
	case schema.TInt:
		return ordApplies(c.op, cmpInt(v.Int, t.i))
	case schema.TFloat, schema.TDuration:
		return ordApplies(c.op, cmpFloat(v.Num, t.f))
	case schema.TTimestamp:
		return ordApplies(c.op, cmpTime(v.Time, t.t))
	case schema.TBool:
		return v.Bool == t.b
	case schema.TJSON:
		return strings.Contains(strings.ToLower(v.Str), t.s)
	default:
		switch c.op {
		case "=":
			return v.Str == t.s
		case "!=":
			return v.Str != t.s
		case "in", "not-in":
			in := false
			for _, x := range c.targets {
				if v.Str == x.s {
					in = true
					break
				}
			}
			return in == (c.op == "in")
		case "contains":
			return strings.Contains(strings.ToLower(v.Str), t.s)
		}
	}
	return false
}

// Run filters recs (restricted to the query's stream), sorts and limits them.
// now anchors relative ranges; runStart anchors "this-run".
func Run(recs []record.Record, s schema.Schema, q Query, now, runStart time.Time) (Result, error) {
	if err := q.Validate(s); err != nil {
		return Result{}, err
	}
	from, to, err := q.Range.Resolve(now, runStart)
	if err != nil {
		return Result{}, err
	}
	stream := q.Stream
	if stream == "" {
		stream = s.Stream
	}
	src := q.Source
	if src == "" {
		src = s.Source
	}
	src, _ = record.ParseSource(src) // Validate checked q.Source
	key := record.StreamKey{Source: src, Name: stream}
	filters := compile(s, q)
	needle := []byte(strings.ToLower(q.Search))

	var res Result
	res.From = from
	res.To = to
	if to.IsZero() {
		res.To = now
	}

	var matched []*record.Record
	for i := range recs {
		r := &recs[i]
		if stream != "" && r.Key() != key {
			continue
		}
		if q.Service != "" && q.Service != "*" && r.Service != q.Service {
			continue
		}
		res.StreamTotal++
		if r.Time.IsZero() {
			if !from.IsZero() {
				continue
			}
		} else {
			if !from.IsZero() && r.Time.Before(from) {
				continue
			}
			if !to.IsZero() && r.Time.After(to) {
				continue
			}
		}
		if len(needle) > 0 && !containsFold(r.Raw, needle) {
			continue
		}
		ok := true
		for _, f := range filters {
			if !f.match(schema.Convert(r.Fields[f.field], f.sfield)) {
				ok = false
				break
			}
		}
		if ok {
			matched = append(matched, r)
		}
	}
	res.Matched = len(matched)

	res.View = q.view()
	switch res.View {
	case "table":
		runTable(matched, s, q, &res)
		return res, nil
	case "graph":
		g, err := runGraph(matched, s, q, from, to, time.Local)
		if err != nil {
			return Result{}, err
		}
		res.Graph = g
		res.Columns = []Column{}
		res.Rows = []Row{}
		return res, nil
	}

	sortRecords(matched, s, q.Sort)

	limit := q.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	if len(matched) > limit {
		matched = matched[:limit]
		res.Truncated = true
	}

	paths := q.Columns
	if len(paths) == 0 {
		paths = intervalColumns(matched, s)
	}
	if len(paths) == 0 {
		// Nothing matched, so no field is present in the interval: keep the
		// stream's own default columns, or the table loses its headings.
		paths = s.Columns
	}
	fields := make([]schema.Field, len(paths))
	for i, p := range paths {
		f, ok := s.Field(p)
		if !ok {
			f = schema.Field{Path: p, Type: schema.TString, Visualizer: schema.DefaultVisualizer(p, schema.TString)}
		}
		fields[i] = f
		label := f.Label
		if label == "" {
			label = p
		}
		res.Columns = append(res.Columns, Column{Path: p, Label: label, Type: f.Type, Unit: f.Unit, Visualizer: f.Visualizer})
	}
	res.Rows = make([]Row, 0, len(matched))
	for _, r := range matched {
		row := Row{Seq: r.Seq, Time: r.Time, TimeInherited: r.TimeInherited, Cells: make([]Cell, len(fields))}
		for i, f := range fields {
			row.Cells[i] = cell(r.Fields[f.Path], f)
			if s, ok := row.Cells[i].V.(string); ok && f.Visualizer == schema.VSQL && !row.Cells[i].Bad {
				row.Cells[i].SQL = sqlcolor.Tokens(s, sqlfmt.SystemOf(r.Fields))
			}
		}
		res.Rows = append(res.Rows, row)
	}
	return res, nil
}

// intervalColumns is the default column list inferred from the matched
// records: each schema field's Count becomes the number of matched records
// holding the path, and fields with none are dropped. Types come from s.
func intervalColumns(matched []*record.Record, s schema.Schema) []string {
	counts := map[string]int{}
	for _, r := range matched {
		for p := range r.Fields {
			counts[p]++
		}
	}
	fs := make([]schema.Field, 0, len(s.Fields))
	for _, f := range s.Fields {
		if f.Count = counts[f.Path]; f.Count > 0 {
			fs = append(fs, f)
		}
	}
	return schema.DefaultColumns(s.Source, fs)
}

// containsFold reports whether b contains the lower-case needle, ignoring
// case. An ASCII needle takes a fast path that folds only ASCII and does not
// allocate; a needle with any non-ASCII byte lowercases b with Unicode rules,
// the same way the needle was lowercased.
func containsFold(b, needle []byte) bool {
	for _, c := range needle {
		if c >= utf8.RuneSelf {
			return bytes.Contains(bytes.ToLower(b), needle)
		}
	}
	n := len(needle)
	for i := 0; i+n <= len(b); i++ {
		j := 0
		for ; j < n; j++ {
			c := b[i+j]
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != needle[j] {
				break
			}
		}
		if j == n {
			return true
		}
	}
	return false
}

// cell encodes a record value for the browser.
func cell(raw any, f schema.Field) Cell {
	v := schema.Convert(raw, f)
	switch {
	case v.Bad:
		return Cell{V: rawString(raw), Bad: true}
	case v.Empty:
		return Cell{}
	}
	switch f.Type {
	case schema.TInt:
		if v.Int > maxExactInt || v.Int < -maxExactInt {
			return Cell{V: strconv.FormatInt(v.Int, 10)}
		}
		return Cell{V: v.Int}
	case schema.TFloat, schema.TDuration:
		return Cell{V: v.Num}
	case schema.TTimestamp:
		return Cell{V: v.Time.UTC().Format(time.RFC3339Nano)}
	case schema.TBool:
		return Cell{V: v.Bool}
	case schema.TJSON:
		// JSON text, not the decoded value: the browser's JSON.parse would
		// round integers above 2^53, and json.Marshal keeps json.Number digits.
		return Cell{V: rawString(raw)}
	}
	return Cell{V: v.Str}
}

func rawString(raw any) string {
	switch x := raw.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return ""
	}
	return string(b)
}

type sortKey struct {
	missing bool // bad or empty: sorts last in either direction
	i       int64
	f       float64
	s       string
	t       time.Time
	b       bool
}

func sortRecords(recs []*record.Record, s schema.Schema, so Sort) {
	field, desc := so.Field, so.Desc
	if field == "" {
		field, desc = "timestamp", true
	}
	if field == "timestamp" { // the effective time, so inherited rows stay in place
		sort.SliceStable(recs, func(a, b int) bool {
			if c := cmpTime(recs[a].Time, recs[b].Time); c != 0 {
				return (c < 0) != desc
			}
			return (recs[a].Seq < recs[b].Seq) != desc
		})
		return
	}
	f, ok := s.Field(field)
	if !ok {
		f = schema.Field{Path: field, Type: schema.TString}
	}
	keys := make(map[*record.Record]sortKey, len(recs))
	for _, r := range recs {
		v := schema.Convert(r.Fields[field], f)
		k := sortKey{missing: v.Bad || v.Empty}
		if !k.missing {
			switch f.Type {
			case schema.TInt:
				k.i = v.Int
			case schema.TFloat, schema.TDuration:
				k.f = v.Num
				if math.IsNaN(k.f) {
					k.missing = true
				}
			case schema.TTimestamp:
				k.t = v.Time
			case schema.TBool:
				k.b = v.Bool
			default:
				k.s = v.Str
			}
		}
		keys[r] = k
	}
	cmp := func(x, y sortKey) int {
		switch f.Type {
		case schema.TInt:
			return cmpInt(x.i, y.i)
		case schema.TFloat, schema.TDuration:
			return cmpFloat(x.f, y.f)
		case schema.TTimestamp:
			return cmpTime(x.t, y.t)
		case schema.TBool:
			switch {
			case x.b == y.b:
				return 0
			case !x.b:
				return -1
			}
			return 1
		}
		return strings.Compare(x.s, y.s)
	}
	sort.SliceStable(recs, func(a, b int) bool {
		ka, kb := keys[recs[a]], keys[recs[b]]
		if ka.missing != kb.missing {
			return kb.missing // missing last in both directions
		}
		if !ka.missing {
			if c := cmp(ka, kb); c != 0 {
				return (c < 0) != desc
			}
		}
		return (recs[a].Seq < recs[b].Seq) != desc
	})
}
