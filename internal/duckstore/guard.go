package duckstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// allowedTableFunctions are the table functions SQL from the command line
// may call besides the duckdb_* catalog functions: none of them reads or
// writes files or runs other SQL.
var allowedTableFunctions = map[string]bool{
	"range": true, "generate_series": true, "unnest": true, "json_each": true, "json_tree": true,
}

// modelSchemas are the schemas a query may read tables from: the table model
// and DuckDB's catalog views.
var modelSchemas = map[string]bool{
	"logs": true, "spans": true, "loghorn": true, "information_schema": true, "pg_catalog": true,
}

// CheckReadOnly lets q through only when DuckDB's parser reads it as exactly
// one query (SELECT, WITH, FROM-first, DESCRIBE, SHOW, SUMMARIZE or a set
// operation of them) whose tables are the table model's or DuckDB's catalog
// views and whose table functions read no files. Everything else, including
// writes, ATTACH, INSTALL, SET, COPY and several statements, is refused.
func CheckReadOnly(ctx context.Context, db *sql.DB, q string) error {
	var text string
	if err := db.QueryRowContext(ctx, "SELECT CAST(json_serialize_sql(?) AS VARCHAR)", q).Scan(&text); err != nil {
		return err
	}
	var tree struct {
		Error      bool              `json:"error"`
		ErrorType  string            `json:"error_type"`
		Message    string            `json:"error_message"`
		Statements []json.RawMessage `json:"statements"`
	}
	if err := json.Unmarshal([]byte(text), &tree); err != nil {
		return err
	}
	if tree.Error && tree.ErrorType == "parser" {
		return errors.New(tree.Message)
	}
	if tree.Error || len(tree.Statements) != 1 {
		return errors.New("only one query (SELECT, WITH, FROM, DESCRIBE, SHOW or SUMMARIZE) is allowed; it reads the logs and spans tables")
	}
	var node any
	if err := json.Unmarshal(tree.Statements[0], &node); err != nil {
		return err
	}
	ctes := map[string]bool{}
	collectCTEs(node, ctes)
	return checkRefs(node, ctes)
}

// collectCTEs adds the name of every common table expression in x.
func collectCTEs(x any, ctes map[string]bool) {
	switch v := x.(type) {
	case map[string]any:
		if m, ok := v["cte_map"].(map[string]any); ok {
			if list, ok := m["map"].([]any); ok {
				for _, e := range list {
					if e, ok := e.(map[string]any); ok {
						if k, ok := e["key"].(string); ok {
							ctes[strings.ToLower(k)] = true
						}
					}
				}
			}
		}
		for _, c := range v {
			collectCTEs(c, ctes)
		}
	case []any:
		for _, c := range v {
			collectCTEs(c, ctes)
		}
	}
}

// checkRefs refuses a table function or table q may not read.
func checkRefs(x any, ctes map[string]bool) error {
	switch v := x.(type) {
	case map[string]any:
		switch v["type"] {
		case "TABLE_FUNCTION":
			f, _ := v["function"].(map[string]any)
			name, _ := f["function_name"].(string)
			if !allowedTableFunctions[strings.ToLower(name)] && !strings.HasPrefix(strings.ToLower(name), "duckdb_") {
				return fmt.Errorf("table function %s is not allowed: read the logs and spans tables", name)
			}
		case "BASE_TABLE":
			catalog, _ := v["catalog_name"].(string)
			sch, _ := v["schema_name"].(string)
			table, _ := v["table_name"].(string)
			ok := (catalog == "" || strings.EqualFold(catalog, "memory")) && modelSchemas[strings.ToLower(sch)]
			if catalog == "" && sch == "" && ctes[strings.ToLower(table)] {
				ok = true
			}
			if !ok {
				name := table
				if sch != "" {
					name = sch + "." + name
				}
				if catalog != "" {
					name = catalog + "." + name
				}
				return fmt.Errorf("table %q is not allowed: read logs.\"<stream>\" or spans.\"<stream>\" (SELECT * FROM loghorn.tables lists them)", name)
			}
		}
		for _, c := range v {
			if err := checkRefs(c, ctes); err != nil {
				return err
			}
		}
	case []any:
		for _, c := range v {
			if err := checkRefs(c, ctes); err != nil {
				return err
			}
		}
	}
	return nil
}
