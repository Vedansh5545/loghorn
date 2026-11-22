package sqlfmt

import (
	"strings"

	"github.com/ajitpratap0/GoSQLX/pkg/highlight"
	"github.com/ajitpratap0/GoSQLX/pkg/sql/keywords"
)

// maxWidth is the line length past which reflow breaks a formatted line.
// GoSQLX puts each clause on one line however long it gets: its LineWidth
// option is not read by its renderer.
const maxWidth = 80

// headWords may lead a clause line; a line led by them puts them on a line of
// their own, with its pieces indented below.
var headWords = map[string]bool{
	"SELECT": true, "DISTINCT": true, "ALL": true, "FROM": true, "WHERE": true,
	"GROUP": true, "ORDER": true, "BY": true, "HAVING": true, "SET": true,
	"VALUES": true, "RETURNING": true,
}

// reflow breaks each line of formatted SQL longer than width at its top-level
// commas (after them) and AND/OR (before them): those outside parentheses,
// CASE ... END, strings, comments and a BETWEEN's own AND. When the rest of
// the line after its clause keywords is one parenthesized group, as Drizzle
// writes a WHERE, the breaks are one level inside it. A line led by clause
// keywords (headWords) keeps them on a line of their own; any other line, a
// JOIN for instance, keeps its first piece. Pieces are indented two spaces
// past the line, and a piece still longer than width is broken the same way.
// A line with no break point is left as it is.
func reflow(sql string, width int, dialect string) string {
	var opts []highlight.Option
	if dialect != "" {
		opts = append(opts, highlight.WithDialect(keywords.SQLDialect(dialect)))
	}
	lines := strings.Split(sql, "\n")
	var out []string
	for _, line := range lines {
		if len(line) <= width {
			out = append(out, line)
			continue
		}
		out = append(out, breakLine(line, width, opts)...)
	}
	return strings.Join(out, "\n")
}

func breakLine(line string, width int, opts []highlight.Option) []string {
	body := strings.TrimLeft(line, " \t")
	indent := line[:len(line)-len(body)]
	spans := highlight.Spans(body, opts...)

	headEnd := 0
	for _, s := range spans {
		if s.Category != highlight.Keyword || !allHeadWords(body[s.Start:s.End]) {
			break
		}
		headEnd = s.End
	}

	// lead is where the line's own content starts: after its clause
	// keywords, or after a leading AND/OR, which stays with its piece.
	lead := headEnd
	if headEnd == 0 && len(spans) > 0 && spans[0].Category == highlight.Keyword {
		if w := strings.ToUpper(body[spans[0].Start:spans[0].End]); w == "AND" || w == "OR" {
			lead = spans[0].End
		}
	}
	level := 0
	if wrapped(body, spans, lead) {
		level = 1
	}
	var cuts []int // byte offsets in body where a new piece starts
	depth, between := 0, false
	for _, s := range spans {
		text := body[s.Start:s.End]
		switch s.Category {
		case highlight.String, highlight.Comment, highlight.QuotedIdentifier, highlight.Identifier:
			continue
		}
		word := strings.ToUpper(text)
		switch {
		case text == "(":
			depth++
		case text == ")":
			if depth > 0 {
				depth--
			}
		case s.Category == highlight.Keyword && word == "CASE":
			depth++
		case s.Category == highlight.Keyword && word == "END" && depth > 0:
			depth--
		case depth != level || s.Start <= lead:
		case s.Category == highlight.Keyword && word == "BETWEEN":
			between = true
		case text == ",":
			cuts = append(cuts, s.End)
		case s.Category == highlight.Keyword && (word == "AND" || word == "OR"):
			if word == "AND" && between {
				between = false
				continue
			}
			cuts = append(cuts, s.Start)
		}
	}
	if len(cuts) == 0 {
		if sub := openSubquery(body, indent, spans, width, opts); sub != nil {
			return sub
		}
		return []string{line}
	}

	var out []string
	start := 0
	if headEnd > 0 {
		out = append(out, indent+strings.TrimSpace(body[:headEnd]))
		start = headEnd
	}
	pieces := []string{}
	for _, c := range cuts {
		pieces = append(pieces, strings.TrimSpace(body[start:c]))
		start = c
	}
	pieces = append(pieces, strings.TrimSpace(body[start:]))
	for i, p := range pieces {
		if p == "" {
			continue
		}
		pl := indent + "  " + p
		if i == 0 && headEnd == 0 {
			pl = indent + p
		}
		if len(pl) > width && len(p) < len(body) {
			out = append(out, breakLine(pl, width, opts)...)
			continue
		}
		out = append(out, pl)
	}
	return out
}

// wrapped reports whether body after lead is a single parenthesized group,
// allowing the closing parentheses of enclosing groups and a semicolon after
// it.
func wrapped(body string, spans []highlight.Span, lead int) bool {
	depth, open, closedAt := 0, false, -1
	for _, s := range spans {
		if s.Start < lead {
			continue
		}
		text := body[s.Start:s.End]
		if s.Category == highlight.String || s.Category == highlight.Comment || s.Category == highlight.QuotedIdentifier {
			if !open || closedAt >= 0 {
				return false
			}
			continue
		}
		switch {
		case closedAt >= 0:
			if text != ";" && text != ")" {
				return false
			}
		case !open:
			if text != "(" {
				return false
			}
			open, depth = true, 1
		case text == "(":
			depth++
		case text == ")":
			depth--
			if depth == 0 {
				closedAt = s.End
			}
		}
	}
	return closedAt >= 0
}

func allHeadWords(text string) bool {
	words := strings.Fields(strings.ToUpper(text))
	for _, w := range words {
		if !headWords[w] {
			return false
		}
	}
	return len(words) > 0
}

// clauseWords start a clause of a subquery that openSubquery puts on a line
// of its own; joinWords start a join, which also gets its own line.
var (
	clauseWords = map[string]bool{
		"SELECT": true, "FROM": true, "WHERE": true, "GROUP": true, "ORDER": true,
		"HAVING": true, "LIMIT": true, "OFFSET": true, "WINDOW": true,
		"UNION": true, "INTERSECT": true, "EXCEPT": true,
	}
	joinWords = map[string]bool{
		"JOIN": true, "INNER": true, "LEFT": true, "RIGHT": true, "FULL": true,
		"OUTER": true, "CROSS": true, "NATURAL": true,
	}
)

// openSubquery lays out the first parenthesized SELECT (or WITH) in a line
// that has no other break point: the line up to its opening parenthesis, then
// the subquery one clause per line, indented two spaces past the line, with
// whatever follows the subquery (its closing parenthesis, a semicolon) left
// on the last clause. A clause line longer than width is broken like any
// other. It returns nil when the line has no subquery.
func openSubquery(body, indent string, spans []highlight.Span, width int, opts []highlight.Option) []string {
	open := -1
	for i := 0; i+1 < len(spans); i++ {
		if body[spans[i].Start:spans[i].End] != "(" {
			continue
		}
		next := spans[i+1]
		if w := strings.ToUpper(body[next.Start:next.End]); next.Category == highlight.Keyword && (w == "SELECT" || w == "WITH") {
			open = spans[i].End
			break
		}
	}
	if open < 0 {
		return nil
	}
	sub := strings.TrimSpace(body[open:])
	out := []string{indent + strings.TrimSpace(body[:open])}
	inner := indent + "  "
	for _, clause := range splitClauses(sub, opts) {
		l := inner + clause
		if len(l) > width && len(clause) < len(body) {
			out = append(out, breakLine(l, width, opts)...)
			continue
		}
		out = append(out, l)
	}
	return out
}

// splitClauses cuts a SELECT before each of its own clause keywords and
// joins: those outside parentheses and CASE ... END, and before the
// parenthesis that closes the subquery, if any.
func splitClauses(sql string, opts []highlight.Option) []string {
	spans := highlight.Spans(sql, opts...)
	var cuts []int
	depth, prevJoin := 0, false
	for _, s := range spans {
		text := sql[s.Start:s.End]
		if s.Category == highlight.Comment {
			continue
		}
		word := strings.ToUpper(text)
		if i := strings.IndexAny(word, " \t\n"); i > 0 {
			word = word[:i] // the highlighter gives ORDER BY, LEFT JOIN as one span
		}
		isJoin := s.Category == highlight.Keyword && joinWords[word]
		switch {
		case text == "(":
			depth++
		case text == ")":
			depth--
		case s.Category == highlight.Keyword && word == "CASE":
			depth++
		case s.Category == highlight.Keyword && word == "END" && depth > 0:
			depth--
		}
		if depth < 0 {
			break
		}
		if depth == 0 && s.Start > 0 && s.Category == highlight.Keyword &&
			(clauseWords[word] || isJoin && !prevJoin) {
			cuts = append(cuts, s.Start)
		}
		prevJoin = isJoin
	}
	var out []string
	start := 0
	for _, c := range cuts {
		if p := strings.TrimSpace(sql[start:c]); p != "" {
			out = append(out, p)
		}
		start = c
	}
	return append(out, strings.TrimSpace(sql[start:]))
}
