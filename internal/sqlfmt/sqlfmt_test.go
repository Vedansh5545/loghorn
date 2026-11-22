package sqlfmt

import (
	"strings"
	"sync"
	"testing"
)

const joinQuery = `select "events"."logo_image_id", "orgs"."icon_image_id" from "events" inner join "orgs" on "orgs"."id" = "events"."org_id" where "events"."id" = ? limit ?`

func TestFormatLaysOutClausesWithJoinsFlush(t *testing.T) {
	out, err := Format(joinQuery, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"SELECT ", "\nFROM ", "\nINNER JOIN ", "\nWHERE ", "\nLIMIT "} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

// With no known database, Format still copes with ? placeholders, which the
// generic dialect rejects.
func TestFormatUnknownSystemFallsBack(t *testing.T) {
	for _, system := range []string{"", "mongodb", "other_sql"} {
		if _, err := Format(joinQuery, system); err != nil {
			t.Errorf("system %q: %v", system, err)
		}
	}
}

func TestFormatReportsParseErrors(t *testing.T) {
	if _, err := Format("select from where (", "postgresql"); err == nil {
		t.Fatal("want a parse error")
	}
	if out, err := Format("   ", ""); err == nil || out != "" {
		t.Fatalf("blank: %q, %v", out, err)
	}
}

func TestFormatIsSafeConcurrently(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := Format(joinQuery, "sqlite"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

func TestDialect(t *testing.T) {
	for system, want := range map[string]string{
		"postgresql": "postgresql", "mysql": "mysql", "mariadb": "mariadb", "sqlite": "sqlite",
		"microsoft.sql_server": "sqlserver", "mssql": "sqlserver", "oracle.db": "oracle", "oracle": "oracle",
		"clickhouse": "clickhouse", "aws.redshift": "redshift", "redshift": "redshift", "snowflake": "snowflake",
		"gcp.bigquery": "bigquery", "PostgreSQL": "postgresql", "mongodb": "", "": "",
	} {
		if got := Dialect(system); got != want {
			t.Errorf("Dialect(%q)=%q want %q", system, got, want)
		}
	}
}

func TestSystemOf(t *testing.T) {
	for _, c := range []struct {
		fields map[string]any
		want   string
	}{
		{map[string]any{"db.system.name": "sqlite"}, "sqlite"},
		{map[string]any{"db.system": "mysql"}, "mysql"},
		{map[string]any{"otel.attributes.db.system.name": "postgresql"}, "postgresql"},
		{map[string]any{"db.system.name": "sqlite", "db.system": "mysql"}, "sqlite"},
		{map[string]any{"x": 1}, ""},
	} {
		if got := SystemOf(c.fields); got != c.want {
			t.Errorf("SystemOf(%v)=%q want %q", c.fields, got, c.want)
		}
	}
}
