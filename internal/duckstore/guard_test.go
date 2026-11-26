package duckstore

import (
	"context"
	"strings"
	"testing"
)

func TestCheckReadOnly(t *testing.T) {
	g := newRig(t)
	ok := []string{
		`SELECT 1`,
		`WITH a AS (SELECT 1) SELECT * FROM a`,
		`FROM range(3)`,
		`DESCRIBE logs."app"`,
		`SHOW ALL TABLES`,
		`SUMMARIZE logs."app"`,
		`SELECT * FROM information_schema.columns`,
		`SELECT * FROM logs."http.server" JOIN spans."*" USING (seq)`,
		`SELECT * FROM duckdb_columns()`,
		`SELECT loghorn.this_run_id()`,
		`SELECT 1 UNION ALL SELECT 2`,
	}
	for _, q := range ok {
		if err := CheckReadOnly(context.Background(), g.e.DB(), q); err != nil {
			t.Errorf("%s: refused: %v", q, err)
		}
	}
	refused := map[string]string{
		`DELETE FROM x`:                             "only one query",
		`DROP VIEW logs."app"`:                      "only one query",
		`COPY (SELECT 1) TO 'x.csv'`:                "only one query",
		`ATTACH 'x.db'`:                             "only one query",
		`INSTALL httpfs`:                            "only one query",
		`SET threads=1`:                             "only one query",
		`SELECT 1; SELECT 2`:                        "only one query",
		`SELEC 1`:                                   "syntax error",
		`SELECT * FROM read_text('/etc/hosts')`:     "table function read_text",
		`SELECT * FROM query('SELECT 1')`:           "table function query",
		`FROM 'x.csv'`:                              `table "x.csv"`,
		`SELECT * FROM loghorn_db.main.records`:     `table "loghorn_db.main.records"`,
		`SELECT * FROM all_records`:                 `table "all_records"`,
		`SELECT * FROM (SELECT * FROM all_records)`: `table "all_records"`,
		`SELECT (SELECT max(_id) FROM all_records)`: `table "all_records"`,
		`SELECT * FROM logs."app" WHERE seq IN (SELECT _id FROM memory.main.all_records)`: `table "memory.main.all_records"`,
	}
	for q, want := range refused {
		err := CheckReadOnly(context.Background(), g.e.DB(), q)
		if err == nil {
			t.Errorf("%s: allowed", q)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %q lacks %q", q, err, want)
		}
	}
}
