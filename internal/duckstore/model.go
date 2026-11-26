package duckstore

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/schema"
)

// ModelTable is one table of the SQL table model (query.SQLHeader): the
// records of Source and Stream (record.AllStreams: every stream, the table
// "*"), with a typed column per field.
type ModelTable struct {
	Source, Stream string
	Fields         []schema.Field
}

// modelColumns are the model's own columns; a field with one of these names
// (in any case) has no column.
var modelColumns = []string{"seq", "time", "service", "raw"}

// ModelDDL is the statements that make the table model: schemas logs, spans
// and loghorn in the session database, one view per table over the records
// and their archives, and the macros loghorn.this_run_start() and
// loghorn.this_run_id() (NULL when runID is ""). Schemas are dropped and
// made again, so a table that is gone loses its view.
func ModelDDL(tables []ModelTable, runStart time.Time, runID string) []string {
	id := "CAST(NULL AS VARCHAR)"
	if runID != "" {
		id = lit(runID)
	}
	ddl := []string{
		"DROP SCHEMA IF EXISTS memory.logs CASCADE",
		"DROP SCHEMA IF EXISTS memory.spans CASCADE",
		"DROP SCHEMA IF EXISTS memory.loghorn CASCADE",
		"CREATE SCHEMA memory.logs",
		"CREATE SCHEMA memory.spans",
		"CREATE SCHEMA memory.loghorn",
		"CREATE MACRO memory.loghorn.this_run_start() AS TIMESTAMPTZ " + lit(runStart.UTC().Format("2006-01-02 15:04:05.999999")+"+00"),
		"CREATE MACRO memory.loghorn.this_run_id() AS " + id,
	}
	seen := map[string]bool{}
	for _, t := range tables {
		sch := "logs"
		if t.Source == record.SourceOtel {
			sch = "spans"
		}
		// DuckDB names are case-insensitive: the first table of a name wins
		// (the caller puts the larger stream first).
		if k := sch + "." + strings.ToLower(t.Stream); !seen[k] {
			seen[k] = true
			ddl = append(ddl, "CREATE VIEW memory."+sch+"."+ident(t.Stream)+" AS "+modelSelect(t))
		}
	}
	return append(ddl, tablesView)
}

// tablesView lists the model's tables for discovery: name, ready to paste
// (logs."app"), and columns, each as "name TYPE" with the name quoted
// where SQL needs it.
const tablesView = `CREATE VIEW memory.loghorn.tables AS
SELECT c.schema_name || '."' || replace(c.table_name, '"', '""') || '"' AS name,
  string_agg(CASE WHEN regexp_full_match(c.column_name, '[a-z_][a-z0-9_]*')
      AND c.column_name NOT IN (SELECT keyword_name FROM duckdb_keywords() WHERE keyword_category <> 'unreserved')
    THEN c.column_name ELSE '"' || replace(c.column_name, '"', '""') || '"' END
    || ' ' || replace(c.data_type, 'TIMESTAMP WITH TIME ZONE', 'TIMESTAMPTZ'), ', ' ORDER BY c.column_index) AS columns
FROM duckdb_columns() c
WHERE c.database_name = 'memory' AND c.schema_name IN ('logs', 'spans')
GROUP BY c.schema_name, c.table_name
ORDER BY c.schema_name, c.table_name`

// modelSelect is the body of t's view.
func modelSelect(t ModelTable) string {
	// DuckDB names are case-insensitive: of fields whose names differ only
	// in case, the one with the most values gets the column.
	best := map[string]schema.Field{}
	for _, f := range t.Fields {
		k := strings.ToLower(f.Path)
		if b, ok := best[k]; !ok || f.Count > b.Count {
			best[k] = f
		}
	}
	for _, c := range modelColumns {
		delete(best, c)
	}
	var cols []schema.Field
	for _, f := range t.Fields {
		if b, ok := best[strings.ToLower(f.Path)]; ok && b.Path == f.Path {
			cols = append(cols, f)
		}
	}
	out := []string{"_id AS seq", `CAST(timezone('UTC', _time) AS TIMESTAMPTZ) AS "time"`, "_service AS service", "_text AS raw"}
	for i, f := range cols {
		out = append(out, modelValue("v"+itoa(i), f.Type)+" AS "+ident(f.Path))
	}
	return "SELECT " + strings.Join(out, ", ") + " FROM (" + relation(t.Source, "*", t.Stream, cols, lit) + ")"
}

// modelValue is the model's value of v, Relation's value of a field of type
// t: durations in milliseconds rounded to 15 significant digits (as
// query.Round15), times as TIMESTAMPTZ, JSON as JSON.
func modelValue(v string, t schema.Type) string {
	switch norm(t) {
	case schema.TDuration:
		return "(CASE WHEN " + v + " IS NOT NULL THEN CAST(format('{:.15g}', " + v + " * 1000) AS DOUBLE) END)"
	case schema.TTimestamp:
		return "CAST(timezone('UTC', " + v + ") AS TIMESTAMPTZ)"
	case schema.TJSON:
		return "TRY_CAST(" + v + " AS JSON)"
	}
	return v
}

// ReadOnly runs fn on a connection of its own inside a read-only
// transaction, so nothing fn runs can write.
func (e *Engine) ReadOnly(ctx context.Context, fn func(conn *sql.Conn) error) error {
	conn, err := e.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN TRANSACTION READ ONLY"); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	return fn(conn)
}

// WithModel makes the table model of ddl (ModelDDL), unless it is what was
// made last, and runs fn while no other call can replace it.
func (e *Engine) WithModel(ctx context.Context, ddl []string, fn func() error) error {
	text := strings.Join(ddl, ";\n")
	e.modelMu.Lock()
	if text != e.model {
		if err := e.applyModel(ctx, ddl); err != nil {
			e.model = ""
			e.modelMu.Unlock()
			return err
		}
		e.model = text
	}
	e.modelMu.Unlock()
	e.modelMu.RLock()
	defer e.modelMu.RUnlock()
	return fn()
}

func (e *Engine) applyModel(ctx context.Context, ddl []string) error {
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, s := range ddl {
		if _, err := tx.ExecContext(ctx, s); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// LatestRun is the run of the newest record that has a loghorn.run.id, and
// the time of that run's first record; ok is false when no record has one.
func (e *Engine) LatestRun(ctx context.Context) (id string, start time.Time, ok bool) {
	q := `WITH runs AS (SELECT json_extract_string(attributes, ` + lit(jsonPointer("loghorn.run.id")) + `) AS run, _time, _id
		FROM ` + sessionSchema + `all_records WHERE _time IS NOT NULL)
		SELECT run, (SELECT min(_time) FROM runs r2 WHERE r2.run = r.run) FROM runs r
		WHERE run IS NOT NULL ORDER BY _time DESC, _id DESC LIMIT 1`
	var run sql.NullString
	var t sql.NullTime
	if err := e.db.QueryRowContext(ctx, q).Scan(&run, &t); err != nil || !run.Valid || !t.Valid {
		return "", time.Time{}, false
	}
	return run.String, t.Time.UTC(), true
}
