// Package sqlcolor splits SQL into classed tokens for colouring, with the
// chroma SQL lexer and token classes the removed terminal UI used.
package sqlcolor

import (
	"sync"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
)

// Generic SQL: loghorn doesn't know which database wrote the statement.
var lexer = chroma.Coalesce(lexers.Get("sql"))

// Token is a run of SQL text and its class: "kw" keyword, "type" data type,
// "param" bind parameter, "str" string, "num" number, "com" comment, or ""
// for anything else.
type Token struct {
	Class string `json:"c,omitempty"`
	Text  string `json:"t"`
}

// cacheMax bounds the memo of tokenised statements; logged queries repeat a
// lot, and lexing costs around 0.1ms a statement.
const cacheMax = 4096

var (
	mu    sync.Mutex
	cache = map[string][]Token{}
)

// Tokens splits sql into tokens whose texts concatenate back to sql, with
// adjacent tokens of the same class merged. A lexer failure gives the whole
// statement as one plain token. The result is shared: callers must not
// modify it.
func Tokens(sql string) []Token {
	if sql == "" {
		return nil
	}
	mu.Lock()
	out, ok := cache[sql]
	mu.Unlock()
	if ok {
		return out
	}
	out = tokens(sql)
	mu.Lock()
	if len(cache) >= cacheMax {
		cache = map[string][]Token{}
	}
	cache[sql] = out
	mu.Unlock()
	return out
}

func tokens(sql string) []Token {
	it, err := lexer.Tokenise(nil, sql)
	if err != nil {
		return []Token{{Text: sql}}
	}
	var toks []chroma.Token
	for tok := it(); tok != chroma.EOF; tok = it() {
		if tok.Value != "" {
			toks = append(toks, tok)
		}
	}
	var out []Token
	for i := 0; i < len(toks); i++ {
		tok := toks[i]
		c := class(tok.Type)
		switch {
		case tok.Type == chroma.Operator && tok.Value == "?":
			c = "param"
		case tok.Type == chroma.Error && tok.Value == "$" && i+1 < len(toks) && toks[i+1].Type == chroma.LiteralNumberInteger:
			// The generic lexer has no $n placeholder: it gives "$" as an
			// error and the digits as a number.
			c = "param"
			tok.Value += toks[i+1].Value
			i++
		}
		if n := len(out); n > 0 && out[n-1].Class == c {
			out[n-1].Text += tok.Value
			continue
		}
		out = append(out, Token{Class: c, Text: tok.Value})
	}
	return out
}

func class(t chroma.TokenType) string {
	switch {
	case t.InCategory(chroma.Keyword):
		return "kw"
	case t.InSubCategory(chroma.NameBuiltin): // data types
		return "type"
	case t.InSubCategory(chroma.NameVariable): // bind parameters
		return "param"
	case t == chroma.LiteralStringDouble:
		// A double-quoted name is an identifier in standard SQL, not a string.
		return ""
	case t.InSubCategory(chroma.LiteralString):
		return "str"
	case t.InSubCategory(chroma.LiteralNumber):
		return "num"
	case t.InCategory(chroma.Comment):
		return "com"
	}
	return ""
}
