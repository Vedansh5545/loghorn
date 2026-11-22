package sqlfmt

import (
	"strings"
	"testing"
)

func TestReflowBreaksLongSelectList(t *testing.T) {
	in := "SELECT events.logo_image_id, events.icon_image_id, events.rehearsal_of_event_id, orgs.logo_image_id, orgs.icon_image_id\nFROM events\nLIMIT ?;"
	want := `SELECT
  events.logo_image_id,
  events.icon_image_id,
  events.rehearsal_of_event_id,
  orgs.logo_image_id,
  orgs.icon_image_id
FROM events
LIMIT ?;`
	if got := reflow(in, 80, ""); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestReflowLeavesShortLines(t *testing.T) {
	in := "SELECT a, b\nFROM t\nWHERE a = ? AND b = ?"
	if got := reflow(in, 80, ""); got != in {
		t.Fatalf("changed a short statement:\n%s", got)
	}
}

// Commas and ANDs nested in parentheses or CASE, or inside strings, are not
// break points; AND/OR break before, and a trailing semicolon stays put.
func TestReflowOnlyTopLevelBreaks(t *testing.T) {
	in := "WHERE coalesce(a, b, c) = 'x, y AND z' AND CASE WHEN p AND q THEN 1 ELSE 0 END = 1 OR d IN (1, 2, 3) AND e = ?;"
	want := `WHERE
  coalesce(a, b, c) = 'x, y AND z'
  AND CASE WHEN p AND q THEN 1 ELSE 0 END = 1
  OR d IN (1, 2, 3)
  AND e = ?;`
	if got := reflow(in, 40, ""); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

// A JOIN keeps its first piece on its own line; an indented line keeps its
// indent and indents its pieces under it.
func TestReflowJoinAndIndent(t *testing.T) {
	in := "  INNER JOIN orgs ON orgs.id = events.org_id AND orgs.deleted_at IS NULL AND orgs.kind = ?"
	want := `  INNER JOIN orgs ON orgs.id = events.org_id
    AND orgs.deleted_at IS NULL
    AND orgs.kind = ?`
	if got := reflow(in, 60, ""); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

// A long line with nowhere to break is left as it is.
func TestReflowNoBreakPoint(t *testing.T) {
	in := "WHERE " + strings.Repeat("x", 100) + " = ?"
	if got := reflow(in, 80, ""); got != in {
		t.Fatalf("changed:\n%s", got)
	}
}

func TestFormatReflowsLongLines(t *testing.T) {
	q := `select "events"."logo_image_id", "events"."icon_image_id", "events"."rehearsal_of_event_id", "orgs"."logo_image_id", "orgs"."icon_image_id" from "events" inner join "orgs" on "orgs"."id" = "events"."org_id" where "events"."id" = ? limit ?`
	out, err := Format(q, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(out, "\n") {
		if len(line) > maxWidth {
			t.Errorf("line over %d: %q", maxWidth, line)
		}
	}
	if !strings.HasPrefix(out, "SELECT\n  events.logo_image_id,\n") {
		t.Fatalf("select list not broken:\n%s", out)
	}
}

// A clause whose body is one parenthesized group, as Drizzle writes WHERE,
// breaks one level inside it; a piece still too long is broken again.
func TestReflowWrappedCondition(t *testing.T) {
	in := "WHERE (events.org_id IN (?) AND events.status = ? AND (events.mode = ? OR events.mode IS NULL OR events.kind = ?));"
	want := `WHERE
  (events.org_id IN (?)
  AND events.status = ?
  AND (events.mode = ?
    OR events.mode IS NULL
    OR events.kind = ?));`
	if got := reflow(in, 40, ""); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}
