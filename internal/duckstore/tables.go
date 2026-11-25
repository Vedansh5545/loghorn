package duckstore

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"

	"github.com/jimbeveridge/loghorn/internal/record"
)

// columns are the fixed columns of both signal tables, in table order.
// Every normalized field is in attributes; the common ones are copied to
// their own columns for filtering and trace correlation.
var columns = []struct{ name, typ string }{
	{"_id", "BIGINT"}, {"_time", "TIMESTAMP_NS"}, {"_time_inherited", "BOOLEAN"},
	{"_source", "VARCHAR"}, {"_service", "VARCHAR"}, {"_stream", "VARCHAR"},
	{"trace_id", "VARCHAR"}, {"span_id", "VARCHAR"}, {"parent_span_id", "VARCHAR"},
	{"severity", "VARCHAR"}, {"message", "VARCHAR"}, {"duration_ms", "DOUBLE"},
	{"attributes", "JSON"}, {"_raw", "BLOB"},
}

// columnList is the columns' names, comma separated.
func columnList() string {
	names := make([]string, len(columns))
	for i, c := range columns {
		names[i] = c.name
	}
	return strings.Join(names, ", ")
}

func tableDDL(ref string) string {
	defs := make([]string, len(columns))
	for i, c := range columns {
		defs[i] = c.name + " " + c.typ
	}
	return "CREATE TABLE IF NOT EXISTS " + ref + " (" + strings.Join(defs, ", ") + ")"
}

// fieldsDDL is the per-day tallies of field paths that schema inference
// reads (schema.FromTallies); kind is the record's stream.
func fieldsDDL(ref string) string {
	return "CREATE TABLE IF NOT EXISTS " + ref + " (day INTEGER, service VARCHAR, source VARCHAR, kind VARCHAR, path VARCHAR, n BIGINT, kinds INTEGER, PRIMARY KEY (day, service, source, kind, path))"
}

// tableFor is the table of a signal.
func tableFor(source string) string {
	if source == record.SourceOtel {
		return "spans"
	}
	return "logs"
}

// part is one table holding records: in memory or in a database file.
type part struct {
	alias    string // catalog
	source   string
	writable bool
	file     bool
}

func (p *part) ref() string       { return ident(p.alias) + ".main." + tableFor(p.source) }
func (p *part) fieldsRef() string { return ident(p.alias) + ".main._fields" }

// sessionSchema is the session's own catalog, where the views live.
const sessionSchema = "memory.main."

// rebuildViews replaces all_records and all_fields with views over the
// current parts. Writer side, with schemaMu held or before readers start.
func (e *Engine) rebuildViews() error {
	var recs, fields []string
	seen := map[string]bool{}
	for _, p := range e.parts {
		recs = append(recs, "SELECT "+columnList()+" FROM "+p.ref())
		if !seen[p.alias] {
			seen[p.alias] = true
			fields = append(fields, "SELECT * FROM "+p.fieldsRef())
		}
	}
	recs = append(recs, e.archiveSelects()...)
	for _, q := range []string{
		"CREATE OR REPLACE VIEW " + sessionSchema + "all_records AS " + strings.Join(recs, " UNION ALL "),
		"CREATE OR REPLACE VIEW " + sessionSchema + "all_fields AS " + strings.Join(fields, " UNION ALL "),
	} {
		if err := e.exec(q); err != nil {
			return err
		}
	}
	return nil
}

// attributes is r's fields as one flat JSON object, numbers as written.
func attributes(fields map[string]any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(fields) != nil {
		// A json.Number that is not valid JSON (from YAML, say) is kept as
		// text rather than losing the record's attributes.
		b.Reset()
		enc.Encode(numbersAsText(fields))
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func numbersAsText(fields map[string]any) map[string]any {
	out := make(map[string]any, len(fields))
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out[k] = numberText(fields[k])
	}
	return out
}

func numberText(v any) any {
	switch x := v.(type) {
	case json.Number:
		if !json.Valid([]byte(x)) {
			return string(x)
		}
	case []any:
		out := make([]any, len(x))
		for i, y := range x {
			out[i] = numberText(y)
		}
		return out
	case map[string]any:
		return numbersAsText(x)
	}
	return v
}

// jsonPointer is the JSON pointer of a top-level key.
func jsonPointer(key string) string {
	return "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}
