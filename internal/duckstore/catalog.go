package duckstore

import (
	"context"
	"fmt"

	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/schema"
)

// metaSchema holds each catalog's own bookkeeping tables.
const metaSchema = "_loghorn"

// Shared columns, in table order, then the trailing two.
var sharedCols = []struct{ name, typ string }{
	{"_id", "BIGINT"}, {"_time", "TIMESTAMP_NS"}, {"_time_inherited", "BOOLEAN"},
	{"_source", "VARCHAR"}, {"_service", "VARCHAR"}, {"_stream", "VARCHAR"},
	{"_trace_id", "VARCHAR"}, {"_span_id", "VARCHAR"}, {"_parent_span_id", "VARCHAR"}, {"_name", "VARCHAR"},
	{"_bad", "VARCHAR[]"}, {"_raw", "VARCHAR"},
}

var (
	schemaReserved = []string{metaSchema, "information_schema", "pg_catalog", "temp"}
	viewReserved   = []string{"all_records", "all_fields"}
)

func columnReserved() []string {
	out := make([]string, len(sharedCols))
	for i, c := range sharedCols {
		out[i] = c.name
	}
	return out
}

// catalog is one attached DuckDB database: a day's logs or traces, or the
// in-memory ones.
type catalog struct {
	alias    string
	source   string
	day      int // -1 for memory
	path     string
	writable bool
	schemas  *namer
	tnames   map[string]*namer // per schema: table identifiers
	tables   map[tkey]*table
	order    []*table // creation order
	tallies  map[fkey]*schema.Tally
	changed  map[fkey]bool
}

type tkey struct{ service, key string }

type fkey struct{ service, tkey, kind, path string }

type table struct {
	service, key string // real names
	schema, name string // identifiers
	cols         []*column
	byPath       map[string]*column
	names        *namer
	kinds        map[string]schema.KindSet
}

type column struct {
	path, name string
	typ        schema.Type
}

func (t *table) ref(alias string) string {
	return ident(alias) + "." + ident(t.schema) + "." + ident(t.name)
}

type pair struct{ logs, traces *catalog }

func (p *pair) cat(source string) *catalog {
	if source == record.SourceOtel {
		return p.traces
	}
	return p.logs
}

func (e *Engine) exec(q string, args ...any) error {
	_, err := e.wc.ExecContext(context.Background(), q, args...)
	if err != nil {
		return fmt.Errorf("%w\n%s", err, q)
	}
	return nil
}

// attach attaches path as alias and makes sure its bookkeeping tables
// exist (when writable), then loads its tables.
func (e *Engine) attach(alias, path, source string, day int, writable bool) (*catalog, error) {
	q := "ATTACH " + lit(path) + " AS " + ident(alias)
	if !writable {
		q += " (READ_ONLY)"
	}
	if err := e.exec(q); err != nil {
		return nil, err
	}
	c := &catalog{alias: alias, source: source, day: day, path: path, writable: writable,
		schemas: newNamer(schemaReserved...), tnames: map[string]*namer{}, tables: map[tkey]*table{},
		tallies: map[fkey]*schema.Tally{}, changed: map[fkey]bool{}}
	if writable {
		if err := e.initMeta(c); err != nil {
			e.exec("DETACH " + ident(alias))
			return nil, err
		}
	}
	if err := e.loadMeta(c); err != nil {
		e.exec("DETACH " + ident(alias))
		return nil, err
	}
	e.cats = append(e.cats, c)
	return c, nil
}

func (e *Engine) detach(c *catalog) error {
	for i, x := range e.cats {
		if x == c {
			e.cats = append(e.cats[:i:i], e.cats[i+1:]...)
			break
		}
	}
	return e.exec("DETACH " + ident(c.alias))
}

func (c *catalog) meta(t string) string { return ident(c.alias) + "." + metaSchema + "." + t }

func (e *Engine) initMeta(c *catalog) error {
	for _, q := range []string{
		"CREATE SCHEMA IF NOT EXISTS " + ident(c.alias) + "." + metaSchema,
		"CREATE TABLE IF NOT EXISTS " + c.meta("_tables") + " (service VARCHAR, tkey VARCHAR, schema_name VARCHAR, table_name VARCHAR)",
		"CREATE TABLE IF NOT EXISTS " + c.meta("_columns") + " (schema_name VARCHAR, table_name VARCHAR, path VARCHAR, column_name VARCHAR, type VARCHAR)",
		"CREATE TABLE IF NOT EXISTS " + c.meta("_fields") + " (service VARCHAR, tkey VARCHAR, kind VARCHAR, path VARCHAR, n BIGINT, kinds INTEGER, PRIMARY KEY (service, tkey, kind, path))",
		"CREATE TABLE IF NOT EXISTS " + c.meta("_day") + " (day INTEGER, clean BOOLEAN, dirty BOOLEAN, text_bytes BIGINT)",
	} {
		if err := e.exec(q); err != nil {
			return err
		}
	}
	return nil
}

// loadMeta reads a catalog's tables and columns, and for a writable one its
// tallies.
func (e *Engine) loadMeta(c *catalog) error {
	ctx := context.Background()
	rows, err := e.wc.QueryContext(ctx, "SELECT service, tkey, schema_name, table_name FROM "+c.meta("_tables")+" ORDER BY rowid")
	if err != nil {
		return err
	}
	for rows.Next() {
		t := &table{byPath: map[string]*column{}, names: newNamer(columnReserved()...), kinds: map[string]schema.KindSet{}}
		if err := rows.Scan(&t.service, &t.key, &t.schema, &t.name); err != nil {
			rows.Close()
			return err
		}
		c.addTable(t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	byName := map[[2]string]*table{}
	for _, t := range c.order {
		byName[[2]string{t.schema, t.name}] = t
	}
	rows, err = e.wc.QueryContext(ctx, "SELECT schema_name, table_name, path, column_name, type FROM "+c.meta("_columns")+" ORDER BY rowid")
	if err != nil {
		return err
	}
	for rows.Next() {
		var sch, tn string
		col := &column{}
		var typ string
		if err := rows.Scan(&sch, &tn, &col.path, &col.name, &typ); err != nil {
			rows.Close()
			return err
		}
		col.typ = schema.Type(typ)
		if t := byName[[2]string{sch, tn}]; t != nil {
			t.addColumn(col)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if !c.writable {
		return nil
	}
	rows, err = e.wc.QueryContext(ctx, "SELECT service, tkey, kind, path, n, kinds FROM "+c.meta("_fields"))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k fkey
		var n int64
		var kinds int32
		if err := rows.Scan(&k.service, &k.tkey, &k.kind, &k.path, &n, &kinds); err != nil {
			return err
		}
		c.tallies[k] = &schema.Tally{Count: int(n), Kinds: schema.KindSet(kinds)}
		if t := c.tables[tkey{k.service, k.tkey}]; t != nil {
			t.kinds[k.path] |= schema.KindSet(kinds)
		}
	}
	return rows.Err()
}

func (c *catalog) addTable(t *table) {
	c.schemas.set(t.service, t.schema)
	tn := c.tnames[t.schema]
	if tn == nil {
		tn = newNamer()
		c.tnames[t.schema] = tn
	}
	tn.set(t.key, t.name)
	c.tables[tkey{t.service, t.key}] = t
	c.order = append(c.order, t)
}

func (t *table) addColumn(col *column) {
	t.names.set(col.path, col.name)
	t.cols = append(t.cols, col)
	t.byPath[col.path] = col
}

var bg = context.Background()
