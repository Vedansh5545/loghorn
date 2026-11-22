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
