package jsoncolor

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jimbeveridge/loghorn/internal/sqlcolor"
)

func TestTokens(t *testing.T) {
	in := "{\n  \"a\": \"x \\\"q\\\" :\",\n  \"n\": -1.5e3,\n  \"ok\": [true, false, null],\n  \"o\": {}\n}"
	got := Tokens(in)
	want := []sqlcolor.Token{
		{Text: "{\n  "},
		{Class: "key", Text: `"a"`},
		{Text: ": "},
		{Class: "str", Text: `"x \"q\" :"`},
		{Text: ",\n  "},
		{Class: "key", Text: `"n"`},
		{Text: ": "},
		{Class: "num", Text: "-1.5e3"},
		{Text: ",\n  "},
		{Class: "key", Text: `"ok"`},
		{Text: ": ["},
		{Class: "kw", Text: "true"},
		{Text: ", "},
		{Class: "kw", Text: "false"},
		{Text: ", "},
		{Class: "kw", Text: "null"},
		{Text: "],\n  "},
		{Class: "key", Text: `"o"`},
		{Text: ": {}\n}"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Tokens:\n got %#v\nwant %#v", got, want)
	}
	var b strings.Builder
	for _, tk := range got {
		b.WriteString(tk.Text)
	}
	if b.String() != in {
		t.Fatalf("tokens do not rebuild the input: %q", b.String())
	}
}

func TestTokensNotJSON(t *testing.T) {
	for _, in := range []string{"", "plain text line", "{\"a\": 1"} {
		if got := Tokens(in); got != nil {
			t.Errorf("Tokens(%q) = %v, want nil", in, got)
		}
	}
}

func TestTokensTooBig(t *testing.T) {
	big := "[" + strings.Repeat("1,", MaxBytes) + "1]"
	if got := Tokens(big); got != nil {
		t.Fatalf("Tokens of %d bytes = %d tokens, want nil", len(big), len(got))
	}
}
