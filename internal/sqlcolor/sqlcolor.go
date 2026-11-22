// Package sqlcolor splits SQL into classed tokens for colouring, with the
// GoSQLX highlighter (github.com/ajitpratap0/GoSQLX/pkg/highlight) and the
// token classes the removed terminal UI coloured.
package sqlcolor

import (
	"sync"

	"github.com/ajitpratap0/GoSQLX/pkg/highlight"
	"github.com/ajitpratap0/GoSQLX/pkg/sql/keywords"

	"github.com/jimbeveridge/loghorn/internal/sqlfmt"
)

// Token is a run of SQL text and its class: "kw" keyword, "type" data type,
// "param" bind parameter, "str" string, "num" number, "com" comment, or ""
// for anything else, names included.
type Token struct {
	Class string `json:"c,omitempty"`
	Text  string `json:"t"`
}

// cacheMax bounds the memo of tokenised statements; logged queries repeat a
// lot.
const cacheMax = 4096

type key struct{ sql, system string }

var (
	mu    sync.Mutex
	cache = map[key][]Token{}
)

// Tokens splits sql into tokens whose texts concatenate back to sql, with
// adjacent tokens of the same class merged. system is the db.system.name (or
// db.system) value naming the database, "" when unknown; it picks the
// highlighter's dialect. The result is shared: callers must not modify it.
func Tokens(sql, system string) []Token {
	if sql == "" {
		return nil
	}
	k := key{sql, system}
	mu.Lock()
	out, ok := cache[k]
	mu.Unlock()
	if ok {
		return out
	}
	out = tokens(sql, system)
	mu.Lock()
	if len(cache) >= cacheMax {
		cache = map[key][]Token{}
	}
	cache[k] = out
	mu.Unlock()
	return out
}

func tokens(sql, system string) []Token {
	var opts []highlight.Option
	if d := sqlfmt.Dialect(system); d != "" {
		opts = append(opts, highlight.WithDialect(keywords.SQLDialect(d)))
	}
	var out []Token
	add := func(c, text string) {
		if text == "" {
			return
		}
		if n := len(out); n > 0 && out[n-1].Class == c {
			out[n-1].Text += text
			return
		}
		out = append(out, Token{Class: c, Text: text})
	}
	pos := 0
	for _, s := range highlight.Spans(sql, opts...) {
		if s.Start < pos || s.End > len(sql) || s.End < s.Start {
			continue // never expected; keep the text whole regardless
		}
		add("", sql[pos:s.Start])
		add(class(s.Category), sql[s.Start:s.End])
		pos = s.End
	}
	add("", sql[pos:])
	return out
}

func class(c highlight.Category) string {
	switch c {
	case highlight.Keyword, highlight.Constant: // TRUE, FALSE, NULL read as keywords
		return "kw"
	case highlight.DataType:
		return "type"
	case highlight.Placeholder:
		return "param"
	case highlight.String:
		return "str"
	case highlight.Number:
		return "num"
	case highlight.Comment:
		return "com"
	}
	return ""
}
