package sqlcolor

import (
	"reflect"
	"strings"
	"testing"
)

func TestTokens(t *testing.T) {
	got := Tokens(`select "id", count(*) from "lots" where "lots"."id" = ? and price > 10 and name = 'x' -- note`, "")
	var b strings.Builder
	classes := map[string][]string{}
	for _, tk := range got {
		b.WriteString(tk.Text)
		if tk.Class != "" {
			classes[tk.Class] = append(classes[tk.Class], strings.TrimSpace(tk.Text))
		}
	}
	if b.String() != `select "id", count(*) from "lots" where "lots"."id" = ? and price > 10 and name = 'x' -- note` {
		t.Fatalf("tokens do not rebuild the input: %q", b.String())
	}
	for class, want := range map[string]string{"kw": "select", "num": "10", "str": "'x'", "com": "-- note"} {
		found := false
		for _, v := range classes[class] {
			if strings.EqualFold(v, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("class %s = %q, want it to hold %q", class, classes[class], want)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i].Class == got[i-1].Class {
			t.Fatalf("adjacent tokens share class %q: %+v", got[i].Class, got[i-1:i+1])
		}
	}
}

func TestTokensEmpty(t *testing.T) {
	if got := Tokens("", ""); len(got) != 0 {
		t.Fatalf("Tokens(\"\") = %+v", got)
	}
	if got := Tokens("x", ""); !reflect.DeepEqual(got, []Token{{Text: "x"}}) {
		t.Fatalf("Tokens(x) = %+v", got)
	}
}

// Double-quoted names are identifiers in standard SQL, so they stay plain;
// ? and $n placeholders are bind parameters.
func TestTokensIdentifiersAndParams(t *testing.T) {
	got := Tokens(`select "id" from "lots" where a = ? and b = $12`, "postgresql")
	want := []Token{
		{Class: "kw", Text: "select"}, {Text: ` "id" `}, {Class: "kw", Text: "from"}, {Text: ` "lots" `},
		{Class: "kw", Text: "where"}, {Text: " a = "}, {Class: "param", Text: "?"}, {Text: " "},
		{Class: "kw", Text: "and"}, {Text: " b = "}, {Class: "param", Text: "$12"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}
