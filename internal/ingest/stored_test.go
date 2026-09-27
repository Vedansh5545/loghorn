package ingest

import (
	"reflect"
	"strings"
	"testing"
)

func collectStored(t *testing.T, input string) []string {
	t.Helper()
	var got []string
	if err := Stored(strings.NewReader(input), func(rec []byte) {
		got = append(got, string(rec))
	}); err != nil {
		t.Fatalf("Stored error: %v", err)
	}
	return got
}

// A stored day can hold one run's JSON lines followed by another run's YAML
// documents. Detecting the framing from the first line alone shreds the YAML.
func TestStoredJSONLinesThenYAML(t *testing.T) {
	in := `{"a":1}
{"a":2}
---
insertId: x
jsonPayload:
  message: hello
- item
receiveTimestamp: '2026'
---
insertId: y
`
	want := []string{
		`{"a":1}`,
		`{"a":2}`,
		"---\ninsertId: x\njsonPayload:\n  message: hello\n- item\nreceiveTimestamp: '2026'",
		"---\ninsertId: y",
	}
	if got := collectStored(t, in); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

// A line that can't continue a YAML document ends it and stands alone.
func TestStoredYAMLThenLines(t *testing.T) {
	in := "---\nk: v\n\n  more\n{\"a\":1}\nplain text line\n--- \nk2: v2"
	want := []string{
		"---\nk: v\n\n  more",
		`{"a":1}`,
		"plain text line",
		"--- \nk2: v2",
	}
	if got := collectStored(t, in); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

// Without any YAML a stored file is one record per line, blank lines included.
func TestStoredPlainLines(t *testing.T) {
	want := []string{"a", "", "b: c"}
	if got := collectStored(t, "a\n\nb: c\n"); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}
