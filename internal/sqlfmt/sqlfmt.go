// Package sqlfmt pretty-prints SQL with GoSQLX
// (github.com/ajitpratap0/GoSQLX), which parses the statement and renders it
// one clause per line, with joins flush with FROM. Rendering is from the
// parse tree, so the output can differ from what was logged beyond layout:
// keywords are uppercased, quotes around plain names are dropped and a
// trailing semicolon is added.
package sqlfmt

import (
	"errors"
	"strings"

	"github.com/ajitpratap0/GoSQLX/pkg/formatter"
)

// dialects maps OpenTelemetry db.system.name values, and the older
// db.system ones, to GoSQLX dialect names.
var dialects = map[string]string{
	"postgresql": "postgresql", "mysql": "mysql", "mariadb": "mariadb", "sqlite": "sqlite",
	"microsoft.sql_server": "sqlserver", "mssql": "sqlserver",
	"oracle.db": "oracle", "oracle": "oracle",
	"clickhouse": "clickhouse", "snowflake": "snowflake",
	"aws.redshift": "redshift", "redshift": "redshift",
	"gcp.bigquery": "bigquery", "bigquery": "bigquery",
}

// Dialect is the GoSQLX dialect for a db.system.name (or db.system) value, or
// "" when GoSQLX has none for it.
func Dialect(system string) string { return dialects[strings.ToLower(system)] }

// systemKeys are where a record names its database: the OTel attribute, the
// pre-1.26 name, and both as an OTLP log record's attributes.
var systemKeys = []string{"db.system.name", "db.system", "otel.attributes.db.system.name", "otel.attributes.db.system"}

// SystemOf returns the database system a record's fields name, or "".
func SystemOf(fields map[string]any) string {
	for _, k := range systemKeys {
		if s, _ := fields[k].(string); s != "" {
			return s
		}
	}
	return ""
}

// guesses are the dialects tried, in order, when the database is unknown:
// the generic one first, then dialects that accept ? placeholders, which
// the generic dialect rejects.
var guesses = []string{"", "sqlite", "postgresql", "mysql"}

// Format lays sql out for the database named by system, a db.system.name or
// db.system value ("" when unknown). A known dialect is the only one tried;
// otherwise the first of guesses that parses wins. It returns an error when
// none parses, or for blank input.
func Format(sql, system string) (string, error) {
	if strings.TrimSpace(sql) == "" {
		return "", errors.New("sqlfmt: no SQL")
	}
	tries := guesses
	if d := Dialect(system); d != "" {
		tries = []string{d}
	}
	var first error
	for _, d := range tries {
		out, err := formatter.New(formatter.Options{Dialect: d}).Format(sql)
		if err == nil && strings.TrimSpace(out) != "" {
			return out, nil
		}
		if first == nil {
			first = err
		}
	}
	if first == nil {
		first = errors.New("no output")
	}
	return "", errors.New("sqlfmt: " + first.Error())
}
