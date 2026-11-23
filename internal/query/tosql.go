package query

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/schema"
)

// SQLHeader is the comment block that opens every ToSQL answer: the DuckDB
// table model the statement targets, so a reader (or an LLM editing it)
// knows what the names mean.
const SQLHeader = `-- loghorn query as DuckDB SQL. Table model:
--   Schema logs holds log records and schema spans holds span records, with
--   one table per stream named exactly as the stream: logs."app",
--   spans."http.server". The table "*" (spans."*") is every stream of its
--   schema at once.
--   Columns: seq BIGINT (store order; every sort breaks ties on it),
--   time TIMESTAMPTZ (the effective time, inherited times included; NULL
--   when the record has none), service VARCHAR, raw VARCHAR (the line that
--   search matches), then one column per field path, quoted with its dotted
--   name ("http.status").
--   Field types: string VARCHAR, int BIGINT, float DOUBLE, duration DOUBLE
--   (ms), bool BOOLEAN, time TIMESTAMPTZ, json JSON.
--   A missing value, null or empty string is NULL. A value that does not fit
--   its field's type (a bad value) also loads as NULL: loghorn counts a bad
--   value as not-empty, SQL does not.
--   A field named seq, time, service or raw (in any case) has no column.
--   Macros: loghorn.this_run_start() is the start of the run that is current
--   when the SQL executes, and loghorn.this_run_id() is its run ID.
`

// thisRunNote explains the run-ID condition of a this-run range.
const thisRunNote = `-- this-run: a wrapped command's own output lines carry no run ID, so once
-- later runs are stored this also matches their untagged lines.
`

// ErrSQLView is returned by ToSQL for a view it cannot translate yet.
var ErrSQLView = errors.New("format sql supports only the samples view")

// modelColumns are the table model's own columns; a field path equal to one
// of them (DuckDB identifiers are case-insensitive) has no column.
var modelColumns = []string{"seq", "time", "service", "raw"}

// ToSQL translates q into a DuckDB statement over the table model in
// SQLHeader that selects the same rows Run does. r is the result of running
// q; it supplies what exists only after a run (the samples column list when
// q names none). Anything that cannot be expressed exactly is an error.
func ToSQL(q Query, s schema.Schema, r *Result) (string, error) {
	if err := q.Validate(s); err != nil {
		return "", err
	}
	if q.view() != "samples" {
		return "", fmt.Errorf("view %s: %w", q.view(), ErrSQLView)
	}
	var b sqlBuilder
	from, err := b.source(q, s)
	if err != nil {
		return "", err
	}
	where, err := b.where(q, s)
	if err != nil {
		return "", err
	}

	paths := q.Columns
	if r != nil {
		paths = make([]string, len(r.Columns))
		for i, c := range r.Columns {
			paths[i] = c.Path
		}
	}
	cols := make([]string, len(paths))
	for i, p := range paths {
		if cols[i], err = fieldIdent(p); err != nil {
			return "", err
		}
	}
	if len(cols) == 0 {
		cols = []string{"*"}
	}
	order, err := sqlOrder(q.Sort, s)
	if err != nil {
		return "", err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}

	var out strings.Builder
	out.WriteString(SQLHeader)
	for _, n := range b.notes {
		out.WriteString(n)
	}
	out.WriteString(selectList(cols))
	out.WriteString("FROM " + from + "\n")
	out.WriteString(whereClause(where))
	out.WriteString("ORDER BY " + order + "\n")
	out.WriteString("LIMIT " + strconv.Itoa(limit))
	if q.Offset > 0 {
		out.WriteString("\nOFFSET " + strconv.Itoa(q.Offset))
	}
	out.WriteString(";\n")
	return out.String(), nil
}

// sqlBuilder collects the comment lines a translation adds.
type sqlBuilder struct {
	notes []string
}

// source is the table of q's stream: schema logs or spans, table the stream.
func (b *sqlBuilder) source(q Query, s schema.Schema) (string, error) {
	stream := q.Stream
	if stream == "" {
		stream = s.Stream
	}
	if stream == "" {
		return "", errors.New("sql: no stream")
	}
	src := q.Source
	if src == "" {
		src = s.Source
	}
	src, _ = record.ParseSource(src) // Validate checked q.Source
	sch := "logs"
	if src == record.SourceOtel {
		sch = "spans"
	}
	id, err := quoteIdent(stream)
	if err != nil {
		return "", err
	}
	return sch + "." + id, nil
}

// where is the list of conditions (ANDed) for service, range, search,
// filters and cell, in Run's order.
func (b *sqlBuilder) where(q Query, s schema.Schema) ([]string, error) {
	var conds []string
	if q.Service != "" && q.Service != "*" {
		lit, err := quoteString(q.Service)
		if err != nil {
			return nil, err
		}
		conds = append(conds, "service = "+lit)
	}
	rc, err := b.rangeConds(q.Range)
	if err != nil {
		return nil, err
	}
	conds = append(conds, rc...)
	if q.Search != "" {
		pat, err := likePattern(q.Search)
		if err != nil {
			return nil, err
		}
		conds = append(conds, "raw ILIKE "+pat+` ESCAPE '\'`)
	}
	for _, f := range q.Filters {
		c, err := filterCond(f, s)
		if err != nil {
			return nil, err
		}
		conds = append(conds, c)
	}
	if q.Cell != nil {
		cc, err := cellConds(q.Cell, s)
		if err != nil {
			return nil, err
		}
		conds = append(conds, cc...)
	}
	return conds, nil
}

// rangeConds translates the range as stated: relative stays relative,
// absolute stays absolute, this-run stays symbolic. A record with no time
// is outside any range with a from, as in Run, and inside one with only a to.
func (b *sqlBuilder) rangeConds(rg Range) ([]string, error) {
	var conds []string
	hasFrom := false
	switch f := strings.TrimSpace(rg.From); f {
	case "", "all":
	case "this-run":
		hasFrom = true
		conds = append(conds, `"time" >= loghorn.this_run_start()`,
			`("loghorn.run.id" = loghorn.this_run_id() OR "loghorn.run.id" IS NULL)`)
		b.notes = append(b.notes, thisRunNote)
	default:
		hasFrom = true
		if d, ok := ParseAgo(f); ok {
			iv, err := sqlInterval(d)
			if err != nil {
				return nil, fmt.Errorf("range from %q: %w", rg.From, err)
			}
			conds = append(conds, `"time" >= now() - `+iv)
		} else {
			t, err := time.Parse(time.RFC3339Nano, f)
			if err != nil {
				return nil, fmt.Errorf("range from %q: want this-run, all, a relative time or RFC 3339", rg.From)
			}
			lit, err := timestampLit(t)
			if err != nil {
				return nil, fmt.Errorf("range from %q: %w", rg.From, err)
			}
			conds = append(conds, `"time" >= `+lit)
		}
	}
	switch t := strings.TrimSpace(rg.To); t {
	case "", "now":
	default:
		tm, err := time.Parse(time.RFC3339Nano, t)
		if err != nil {
			return nil, fmt.Errorf("range to %q: want now or RFC 3339", rg.To)
		}
		lit, err := timestampLit(tm)
		if err != nil {
			return nil, fmt.Errorf("range to %q: %w", rg.To, err)
		}
		c := `"time" <= ` + lit
		if !hasFrom {
			c = `("time" IS NULL OR ` + c + ")"
		}
		conds = append(conds, c)
	}
	return conds, nil
}

// sqlInterval writes d as an exact DuckDB interval in hours, or else
// seconds, milliseconds or microseconds. Days are written as hours: a
// DuckDB day is a calendar day, which across a DST change is not 24 h.
func sqlInterval(d time.Duration) (string, error) {
	units := []struct {
		d    time.Duration
		name string
	}{
		{time.Hour, "hours"}, {time.Second, "seconds"},
		{time.Millisecond, "milliseconds"}, {time.Microsecond, "microseconds"},
	}
	for _, u := range units {
		if d%u.d == 0 {
			return "INTERVAL '" + strconv.FormatInt(int64(d/u.d), 10) + " " + u.name + "'", nil
		}
	}
	return "", fmt.Errorf("%v is finer than DuckDB's microseconds", d)
}

// timestampLit is a TIMESTAMPTZ literal in UTC. DuckDB keeps microseconds,
// so a finer instant is an error.
func timestampLit(t time.Time) (string, error) {
	if t.Nanosecond()%1000 != 0 {
		return "", fmt.Errorf("%s is finer than DuckDB's microseconds", t.Format(time.RFC3339Nano))
	}
	return "TIMESTAMPTZ '" + t.UTC().Format(time.RFC3339Nano) + "'", nil
}

// filterCond translates one filter with compiled.match's semantics. An
// empty value (NULL) matches empty, != and not-in, as in Run.
func filterCond(f Filter, s schema.Schema) (string, error) {
	id, err := fieldIdent(f.Field)
	if err != nil {
		return "", err
	}
	t := fieldType(s, f.Field)
	switch f.Op {
	case "empty":
		if t == schema.TString {
			return "(" + id + " IS NULL OR " + id + " = '')", nil
		}
		return id + " IS NULL", nil
	case "not-empty":
		if t == schema.TString {
			return "(" + id + " IS NOT NULL AND " + id + " <> '')", nil
		}
		return id + " IS NOT NULL", nil
	case "contains":
		pat, err := likePattern(f.Values[0])
		if err != nil {
			return "", err
		}
		return id + " ILIKE " + pat + ` ESCAPE '\'`, nil
	case "in", "not-in":
		lits := make([]string, len(f.Values))
		for i, v := range f.Values {
			if lits[i], err = operandLit(t, v); err != nil {
				return "", err
			}
		}
		list := "(" + strings.Join(lits, ", ") + ")"
		if f.Op == "in" {
			return id + " IN " + list, nil
		}
		return "(" + id + " IS NULL OR " + id + " NOT IN " + list + ")", nil
	}
	lit, err := operandLit(t, f.Values[0])
	if err != nil {
		return "", err
	}
	switch f.Op {
	case "=", "<", "<=", ">", ">=":
		return id + " " + f.Op + " " + lit, nil
	case "!=":
		return "(" + id + " IS NULL OR " + id + " <> " + lit + ")", nil
	}
	return "", fmt.Errorf("sql: operator %q has no translation", f.Op)
}

// operandLit is the SQL literal of filter value v for a field of type t, in
// the table model's unit (a duration in ms, rounded as exports round it).
func operandLit(t schema.Type, v string) (string, error) {
	tg, err := parseOperand(t, v)
	if err != nil {
		return "", err
	}
	switch t {
	case schema.TInt:
		return strconv.FormatInt(tg.i, 10), nil
	case schema.TFloat:
		return floatLit(tg.f)
	case schema.TDuration:
		return floatLit(Round15(tg.f * 1000))
	case schema.TTimestamp:
		return timestampLit(tg.t)
	case schema.TBool:
		return strconv.FormatBool(tg.b), nil
	}
	return quoteString(tg.s)
}

func floatLit(f float64) (string, error) {
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if strings.ContainsAny(s, "IN") { // Inf, NaN
		return "", fmt.Errorf("%s has no SQL literal", s)
	}
	return s, nil
}

// cellConds narrows to one heatmap cell as HeatCell.contains does, which
// reads duration_ms as a raw number; only an int or float column holds that.
func cellConds(c *HeatCell, s schema.Schema) ([]string, error) {
	switch fieldType(s, durationField.Path) {
	case schema.TInt, schema.TFloat:
	default:
		return nil, fmt.Errorf("sql: a heatmap cell needs %s typed int or float", durationField.Path)
	}
	from, err := timestampLit(c.From)
	if err != nil {
		return nil, err
	}
	to, err := timestampLit(c.To)
	if err != nil {
		return nil, err
	}
	id, _ := fieldIdent(durationField.Path)
	conds := []string{`"time" >= ` + from, `"time" < ` + to}
	if c.Lo != nil {
		lo, err := floatLit(*c.Lo)
		if err != nil {
			return nil, err
		}
		conds = append(conds, id+" >= "+lo)
	}
	hi, err := floatLit(c.Hi)
	if err != nil {
		return nil, err
	}
	return append(conds, id+" < "+hi), nil
}

// sqlOrder is the ORDER BY list of Run's sort: the effective time, or a
// field with missing values last in both directions (DuckDB's default
// NULLS LAST), ties broken on seq in the same direction.
func sqlOrder(so Sort, s schema.Schema) (string, error) {
	field, desc := so.Field, so.Desc
	if field == "" {
		field, desc = "timestamp", true
	}
	dir := " ASC"
	if desc {
		dir = " DESC"
	}
	if field == "timestamp" {
		// Run sorts a record with no time as the zero time: first ascending.
		if desc {
			return `"time" DESC, seq DESC`, nil
		}
		return `"time" ASC NULLS FIRST, seq ASC`, nil
	}
	id, err := fieldIdent(field)
	if err != nil {
		return "", err
	}
	return id + dir + ", seq" + dir, nil
}

// fieldIdent quotes a field path as a column name. A path that is one of
// the model's own columns has no column of its own.
func fieldIdent(path string) (string, error) {
	for _, m := range modelColumns {
		if strings.EqualFold(path, m) {
			return "", fmt.Errorf("sql: field %q has no column: the table model's %s column takes its name", path, m)
		}
	}
	return quoteIdent(path)
}

// quoteIdent double-quotes an identifier, doubling any ".
func quoteIdent(s string) (string, error) {
	if err := sqlText(s); err != nil {
		return "", err
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`, nil
}

// quoteString single-quotes a string literal, doubling any '. DuckDB string
// literals take backslashes literally.
func quoteString(s string) (string, error) {
	if err := sqlText(s); err != nil {
		return "", err
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'", nil
}

// likePattern is the quoted ILIKE pattern matching v anywhere, with the
// escape character \ and the wildcards % and _ escaped. v is lowercased as
// Run lowercases search and contains values.
func likePattern(v string) (string, error) {
	v = strings.ToLower(v)
	v = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(v)
	return quoteString("%" + v + "%")
}

// sqlText rejects what no DuckDB literal or identifier can hold.
func sqlText(s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("sql: %q is not valid UTF-8", s)
	}
	if strings.IndexByte(s, 0) >= 0 {
		return fmt.Errorf("sql: %q holds a NUL byte", s)
	}
	return nil
}

// selectList writes the SELECT clause: on one line when short, else one
// column per line.
func selectList(cols []string) string {
	one := "SELECT " + strings.Join(cols, ", ")
	if len(one) <= 80 {
		return one + "\n"
	}
	return "SELECT\n  " + strings.Join(cols, ",\n  ") + "\n"
}

// whereClause writes the WHERE clause: one condition on the WHERE line, more
// one per line joined by AND; none writes nothing.
func whereClause(conds []string) string {
	switch len(conds) {
	case 0:
		return ""
	case 1:
		return "WHERE " + conds[0] + "\n"
	}
	return "WHERE\n  " + strings.Join(conds, "\n  AND ") + "\n"
}
