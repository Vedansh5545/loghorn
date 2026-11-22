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

// A long line whose only break points are inside a parenthesized SELECT opens
// the subquery one clause per line, and breaks those clauses in turn.
func TestFormatNestedSubqueries(t *testing.T) {
	q := "select id, event_id, type from lots where (lots.type in (?, ?) and lots.status = ? and lots.close_at > ? and lots.close_at <= ? and not exists (select 1 from events t where t.id = lots.event_id and t.rehearsal_of_event_id is not null and (t.rehearsal_ended_at is not null or not exists (select 1 from events s where s.id = t.rehearsal_of_event_id) or exists (select 1 from lots sa where sa.event_id = t.rehearsal_of_event_id and sa.status <> 'draft'))))"
	out, err := Format(q, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	want := `SELECT id, event_id, type
FROM lots
WHERE
  (lots.type IN (?, ?)
  AND lots.status = ?
  AND lots.close_at > ?
  AND lots.close_at <= ?
  AND NOT EXISTS (
    SELECT 1
    FROM events t
    WHERE
      t.id = lots.event_id
      AND t.rehearsal_of_event_id IS NOT NULL
      AND (t.rehearsal_ended_at IS NOT NULL
        OR NOT EXISTS (
          SELECT 1
          FROM events s
          WHERE s.id = t.rehearsal_of_event_id)
        OR EXISTS (
          SELECT 1
          FROM lots sa
          WHERE
            sa.event_id = t.rehearsal_of_event_id
            AND sa.status <> 'draft'))));`
	if out != want {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}
}

// A subquery's joins start lines of their own, a multi-word join on one
// line; a subquery on a line short enough is left inline.
func TestReflowSubqueryJoins(t *testing.T) {
	in := "WHERE id IN (SELECT o.id FROM orgs o LEFT OUTER JOIN users u ON u.org_id = o.id WHERE u.id = ? ORDER BY o.id)\nAND x IN (SELECT 1)"
	want := `WHERE id IN (
  SELECT o.id
  FROM orgs o
  LEFT OUTER JOIN users u ON u.org_id = o.id
  WHERE u.id = ?
  ORDER BY o.id)
AND x IN (SELECT 1)`
	if got := reflow(in, 60, ""); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}
