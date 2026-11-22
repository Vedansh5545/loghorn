// Package jsoncolor splits JSON text into coloured tokens for the web page's
// record box, in the same shape as sqlcolor's tokens so the page renders
// both the same way.
package jsoncolor

import (
	"encoding/json"

	"github.com/jimbeveridge/loghorn/internal/sqlcolor"
)

// MaxBytes is the largest text Tokens colours; a bigger record shows plain,
// since its tokens would roughly double an already large answer.
const MaxBytes = 256 << 10

// Tokens returns s as tokens whose texts concatenate back to s: object keys
// get class "key", strings "str", numbers "num", and true, false and null
// "kw"; punctuation and whitespace have no class and are merged into one
// token per run. It returns nil when s is not valid JSON or is over MaxBytes.
func Tokens(s string) []sqlcolor.Token {
	if len(s) > MaxBytes || !json.Valid([]byte(s)) {
		return nil
	}
	var out []sqlcolor.Token
	plain := 0 // start of the pending unclassed run
	emit := func(class string, from, to int) {
		if plain < from {
			out = append(out, sqlcolor.Token{Text: s[plain:from]})
		}
		out = append(out, sqlcolor.Token{Class: class, Text: s[from:to]})
		plain = to
	}
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == '"':
			j := i + 1
			for s[j] != '"' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			j++
			class := "str"
			k := j
			for k < len(s) && (s[k] == ' ' || s[k] == '\t' || s[k] == '\n' || s[k] == '\r') {
				k++
			}
			if k < len(s) && s[k] == ':' {
				class = "key"
			}
			emit(class, i, j)
			i = j
		case c == '-' || (c >= '0' && c <= '9'):
			j := i + 1
			for j < len(s) && isNumByte(s[j]) {
				j++
			}
			emit("num", i, j)
			i = j
		case c >= 'a' && c <= 'z':
			j := i + 1
			for j < len(s) && s[j] >= 'a' && s[j] <= 'z' {
				j++
			}
			emit("kw", i, j)
			i = j
		default:
			i++
		}
	}
	if plain < len(s) {
		out = append(out, sqlcolor.Token{Text: s[plain:]})
	}
	return out
}

func isNumByte(c byte) bool {
	return c >= '0' && c <= '9' || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-'
}
