package adapter

import (
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/entry"
)

func TestStripActionsPrefix(t *testing.T) {
	cases := []struct {
		in   string
		ok   bool
		rest string
		ns   int
	}{
		{"2026-11-17T17:00:30.3832868Z {\"a\":1}", true, `{"a":1}`, 383286800},
		{"2026-11-17T17:00:30Z hello", true, "hello", 0},
		{"2026-11-17T17:00:30.1Z x", true, "x", 100000000},
		{"2026-11-17T17:00:30.123456789Z x", true, "x", 123456789},
		{"2026-11-17T17:00:30.3832868 {}", false, "", 0},
		{"2026-11-17T17:00:30.Z x", false, "", 0},
		{"2026-11-17T17:00:30.1234567890Z x", false, "", 0},
		{"2026-11-17T17:00:30Zx", false, "", 0},
		{"abc 2026-11-17T17:00:30Z x", false, "", 0},
		{"2026-13-45T17:00:30Z x", false, "", 0},
		{"", false, "", 0},
	}
	for _, c := range cases {
		rest, tm, ok := StripActionsPrefix([]byte(c.in))
		if ok != c.ok {
			t.Errorf("%q: ok = %v", c.in, ok)
			continue
		}
		if !ok {
			if string(rest) != c.in {
				t.Errorf("%q: rest changed on failure", c.in)
			}
			continue
		}
		if string(rest) != c.rest || tm.Nanosecond() != c.ns || tm.Second() != 30 {
			t.Errorf("%q: rest=%q time=%v", c.in, rest, tm)
		}
	}
}

// A GitHub Actions job log puts a timestamp in front of every line, so an
// app's JSON log line arrives as "<ts> {...}". ParseLine must still read it as
// a LogEntry, keep the JSON's own time, and drop the prefix from Raw.
func TestParseLineActionsPrefixedJSON(t *testing.T) {
	e := ParseLine([]byte(`2026-11-17T17:00:30.3832868Z {"severity":"ERROR","time":"2026-11-17T17:00:30.382Z","message":"boom"}`))
	if e.Format != entry.FormatLogEntry {
		t.Fatalf("Format = %v, want FormatLogEntry", e.Format)
	}
	if e.Severity != entry.SevError || e.Message != "boom" {
		t.Fatalf("Severity=%v Message=%q", e.Severity, e.Message)
	}
	want, _ := time.Parse(time.RFC3339, "2026-11-17T17:00:30.382Z")
	if !e.Timestamp.Equal(want) {
		t.Fatalf("Timestamp = %v, want the JSON time %v", e.Timestamp, want)
	}
	if len(e.Raw) == 0 || e.Raw[0] != '{' {
		t.Fatalf("Raw should start at the JSON, got %q", e.Raw)
	}
}

func TestParseLineActionsPrefixSuppliesMissingTime(t *testing.T) {
	e := ParseLine([]byte(`2026-11-17T17:00:30.5Z {"severity":"INFO","message":"no time"}`))
	want, _ := time.Parse(time.RFC3339Nano, "2026-11-17T17:00:30.5Z")
	if e.Format != entry.FormatLogEntry || !e.Timestamp.Equal(want) {
		t.Fatalf("Format=%v Timestamp=%v, want LogEntry at %v", e.Format, e.Timestamp, want)
	}
}

// GitHub's own lines are plain text; they stay exactly as they were.
func TestParseLineActionsPrefixedPlainUnchanged(t *testing.T) {
	line := "2026-11-17T16:59:03.3117850Z Current runner version: '2.338.2'"
	e := ParseLine([]byte(line))
	if e.Format != entry.FormatRawText || string(e.Raw) != line || e.Malformed {
		t.Fatalf("Format=%v Raw=%q Malformed=%v", e.Format, e.Raw, e.Malformed)
	}
}

// A prefixed line that only looks like JSON is not reinterpreted: it stays
// the whole original line, as plain text, exactly as before.
func TestParseLineActionsPrefixedBrokenJSONUnchanged(t *testing.T) {
	line := `2026-11-17T17:00:30Z {"severity":"ERROR" broken`
	e := ParseLine([]byte(line))
	if e.Format != entry.FormatRawText || string(e.Raw) != line || e.Malformed {
		t.Fatalf("Format=%v Raw=%q Malformed=%v", e.Format, e.Raw, e.Malformed)
	}
}
