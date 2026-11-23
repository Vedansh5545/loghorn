package duckstore

import (
	"context"
	"database/sql"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/schema"
)

// model is what readers know about the views; it is replaced, never
// changed, so a request keeps a consistent picture.
type model struct {
	views  []*view
	tables map[string][]string // catalog alias -> table refs by tid
}

type vkey struct{ source, service, key string }

// view is one stream (source, service, table key) across every attached
// catalog. Its columns hold each path once, cast to its widest type.
type view struct {
	vkey
	name   string
	cols   []*column
	byPath map[string]*column
	parts  []frozenPart // the tables, as of this model
}

// frozenPart is a table's reference and columns, copied so readers never
// see the writer change them.
type frozenPart struct {
	ref    string
	byPath map[string]column
}

type viewPart struct {
	alias string
	t     *table
}

const sessionSchema = "memory.main."

func (v *view) ref() string { return sessionSchema + ident(v.name) }

// rebuildViews regenerates every view from the attached catalogs and
// publishes the new model. Writer side only.
func (e *Engine) rebuildViews() error {
	m := &model{tables: map[string][]string{}}
	byKey := map[vkey]*view{}
	namers := map[*view]*namer{}
	parts := map[*view][]viewPart{}
	for _, c := range e.cats {
		for _, t := range c.order {
			k := vkey{c.source, t.service, t.key}
			v := byKey[k]
			if v == nil {
				v = &view{vkey: k, name: e.vnames.name(c.source + "/" + t.service + "/" + t.key), byPath: map[string]*column{}}
				byKey[k] = v
				namers[v] = newNamer(columnReserved()...)
				m.views = append(m.views, v)
			}
			for _, col := range t.cols {
				if vc := v.byPath[col.path]; vc != nil {
					vc.typ = widest(vc.typ, col.typ)
					continue
				}
				vc := &column{path: col.path, name: namers[v].name(col.path), typ: col.typ}
				v.cols = append(v.cols, vc)
				v.byPath[col.path] = vc
			}
			parts[v] = append(parts[v], viewPart{c.alias, t})
			fp := frozenPart{ref: t.ref(c.alias), byPath: make(map[string]column, len(t.cols))}
			for _, col := range t.cols {
				fp.byPath[col.path] = *col
			}
			v.parts = append(v.parts, fp)
			m.tables[c.alias] = append(m.tables[c.alias], t.ref(c.alias))
		}
	}
	shared := make([]string, 0, len(sharedCols))
	for _, sc := range sharedCols {
		shared = append(shared, ident(sc.name))
	}
	made := map[string]bool{}
	var all []string
	for _, v := range m.views {
		var branches []string
		for _, p := range parts[v] {
			sel := make([]string, 0, len(shared)+len(v.cols))
			sel = append(sel, shared...)
			for _, vc := range v.cols {
				if col := p.t.byPath[vc.path]; col != nil {
					sel = append(sel, conv(ident(col.name), col.typ, schema.Field{Type: vc.typ})+" AS "+ident(vc.name))
				} else {
					sel = append(sel, nullOf(vc.typ)+" AS "+ident(vc.name))
				}
			}
			branches = append(branches, "SELECT "+strings.Join(sel, ", ")+" FROM "+p.t.ref(p.alias))
		}
		if err := e.exec("CREATE OR REPLACE VIEW " + v.ref() + " AS " + strings.Join(branches, " UNION ALL ")); err != nil {
			return err
		}
		made[v.name] = true
	}
	idx := strings.Join(shared[:indexShared], ", ") + ", _tid"
	for _, c := range e.cats {
		all = append(all, "SELECT "+idx+", "+lit(c.alias)+" AS _cat FROM "+c.meta("_records"))
	}
	if len(all) == 0 {
		nulls := make([]string, indexShared)
		for i, sc := range sharedCols[:indexShared] {
			nulls[i] = "CAST(NULL AS " + sc.typ + ") AS " + ident(sc.name)
		}
		all = []string{"SELECT " + strings.Join(nulls, ", ") + ", CAST(NULL AS INTEGER) AS _tid, CAST(NULL AS VARCHAR) AS _cat WHERE false"}
	}
	if err := e.exec("CREATE OR REPLACE VIEW " + sessionSchema + "all_records AS " + strings.Join(all, " UNION ALL ")); err != nil {
		return err
	}
	fields := []string{"SELECT CAST(NULL AS VARCHAR) AS source, CAST(NULL AS VARCHAR) AS service, CAST(NULL AS VARCHAR) AS tkey, CAST(NULL AS VARCHAR) AS kind, CAST(NULL AS VARCHAR) AS path, CAST(NULL AS BIGINT) AS n, CAST(NULL AS INTEGER) AS kinds WHERE false"}
	for _, c := range e.cats {
		fields = append(fields, "SELECT "+lit(c.source)+", service, tkey, kind, path, n, kinds FROM "+c.meta("_fields"))
	}
	if err := e.exec("CREATE OR REPLACE VIEW " + sessionSchema + "all_fields AS " + strings.Join(fields, " UNION ALL ")); err != nil {
		return err
	}
	for name := range e.viewsMade {
		if !made[name] {
			if err := e.exec("DROP VIEW IF EXISTS " + sessionSchema + ident(name)); err != nil {
				return err
			}
		}
	}
	e.viewsMade = made
	e.snap.Store(m)
	return nil
}

// Relation is a SELECT over the records of one stream (record.AllStreams:
// every stream of source) and service ("" or "*": every service). Its
// columns are _id, _time, _time_inherited, _service, _source, _stream, _raw,
// then for each field i: vi, the value converted to cols[i].Type (NULL when
// missing or not convertible), and xi, true when the value is present but
// not convertible ("bad").
func (e *Engine) Relation(source, service, stream string, cols []schema.Field) (string, []any) {
	m := e.snap.Load()
	every := service == "" || service == "*"
	var branches []string
	var args []any
	for _, v := range m.views {
		if v.source != source || (!every && v.service != service) {
			continue
		}
		if stream != record.AllStreams && v.key != stream && !(source == record.SourceOtel && v.key == record.StreamSpan) {
			continue
		}
		// The day tables are read directly (D-022): binding a wide view
		// costs more than the query itself at this size.
		for _, p := range v.parts {
			var sb strings.Builder
			sb.WriteString("SELECT _id, _time, _time_inherited, _service, _source, _stream, _raw")
			for i, f := range cols {
				vi, xi := " AS v"+itoa(i), " AS x"+itoa(i)
				col, ok := p.byPath[f.Path]
				if !ok {
					sb.WriteString(", " + nullOf(norm(f.Type)) + vi + ", false" + xi)
					continue
				}
				c := ident(col.name)
				val := conv(c, col.typ, f)
				bad := "coalesce(list_contains(_bad, " + lit(f.Path) + "), false)"
				if norm(col.typ) != norm(f.Type) {
					bad = "(" + bad + " OR (" + c + " IS NOT NULL AND " + val + " IS NULL))"
				}
				sb.WriteString(", " + val + vi + ", " + bad + xi)
			}
			sb.WriteString(" FROM " + p.ref)
			if stream != record.AllStreams {
				sb.WriteString(" WHERE _stream = ?")
				args = append(args, stream)
			}
			branches = append(branches, sb.String())
		}
	}
	if len(branches) == 0 {
		var sb strings.Builder
		sb.WriteString("SELECT CAST(NULL AS BIGINT) AS _id, CAST(NULL AS TIMESTAMP_NS) AS _time, CAST(NULL AS BOOLEAN) AS _time_inherited, CAST(NULL AS VARCHAR) AS _service, CAST(NULL AS VARCHAR) AS _source, CAST(NULL AS VARCHAR) AS _stream, CAST(NULL AS VARCHAR) AS _raw")
		for i, f := range cols {
			sb.WriteString(", " + nullOf(norm(f.Type)) + " AS v" + itoa(i) + ", false AS x" + itoa(i))
		}
		sb.WriteString(" WHERE false")
		return sb.String(), nil
	}
	return strings.Join(branches, " UNION ALL "), args
}

// Records returns the records of all_records matching where (SQL over its
// columns), oldest _id first, rebuilt from _raw as they were first built.
// all_records is the per-catalog index (D-021); each record's line is then
// read from its own table.
func (e *Engine) Records(ctx context.Context, where string, args ...any) ([]record.Record, error) {
	m := e.snap.Load()
	rows, err := e.db.QueryContext(ctx, "SELECT _id, _tid, _cat FROM "+sessionSchema+"all_records WHERE "+where+" ORDER BY _id", args...)
	if err != nil {
		return nil, err
	}
	type loc struct {
		cat string
		tid int
	}
	ids := map[loc][]string{}
	var order []loc
	for rows.Next() {
		var id int64
		var l loc
		if err := rows.Scan(&id, &l.tid, &l.cat); err != nil {
			rows.Close()
			return nil, err
		}
		if ids[l] == nil {
			order = append(order, l)
		}
		ids[l] = append(ids[l], strconv.FormatInt(id, 10))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []record.Record
	for _, l := range order {
		refs := m.tables[l.cat]
		if l.tid >= len(refs) {
			continue // a table newer than this model
		}
		for chunk := ids[l]; len(chunk) > 0; {
			n := min(len(chunk), 5000)
			recs, err := e.rowsToRecords(ctx, "SELECT _id, _time, _time_inherited, _service, _source, _raw FROM "+refs[l.tid]+" WHERE _id IN ("+strings.Join(chunk[:n], ",")+")")
			if err != nil {
				return nil, err
			}
			out = append(out, recs...)
			chunk = chunk[n:]
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

// rowsToRecords rebuilds the records of a query returning _id, _time,
// _time_inherited, _service, _source and _raw.
func (e *Engine) rowsToRecords(ctx context.Context, q string, args ...any) ([]record.Record, error) {
	rows, err := e.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []record.Record
	for rows.Next() {
		var id int64
		var tm sql.NullTime
		var inh sql.NullBool
		var svc, src, raw string
		if err := rows.Scan(&id, &tm, &inh, &svc, &src, &raw); err != nil {
			return nil, err
		}
		if r, ok := e.Rebuild(id, tm.Time, inh.Bool, svc, src, raw); ok {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

// Rebuild is the record a stored row stands for: its line normalized again,
// with the row's id, time and service. A zero t means no time.
func (e *Engine) Rebuild(id int64, t time.Time, inherited bool, service, source, raw string) (record.Record, bool) {
	r, ok := e.rebuild(source, []byte(raw))
	if !ok {
		return r, false
	}
	r.Seq, r.Service, r.TimeInherited, r.Time = int(id), service, inherited, t
	return r, true
}

// Tallies merges the per-path tallies of one kind (record.AllStreams: every
// kind) of source, for one service ("" or "*": every service).
func (e *Engine) Tallies(source, service, kind string) (map[string]schema.Tally, error) {
	every := service == "" || service == "*"
	rows, err := e.db.Query("SELECT path, CAST(sum(n) AS BIGINT), bit_or(kinds) FROM "+sessionSchema+"all_fields WHERE source = ? AND (? OR service = ?) AND (? OR kind = ?) GROUP BY path",
		source, every, service, kind == record.AllStreams, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]schema.Tally{}
	for rows.Next() {
		var p string
		var n int64
		var k int32
		if err := rows.Scan(&p, &n, &k); err != nil {
			return nil, err
		}
		out[p] = schema.Tally{Count: int(n), Kinds: schema.KindSet(k)}
	}
	return out, rows.Err()
}
