package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
	_ "time/tzdata" // America/Los_Angeles without relying on the host's zoneinfo

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/store"
)

// logDir is where the log file lives and -historical reads from: .loghorn
// under the directory loghorn was started in, one per project.
func TestLogDir(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	got, err := logDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cwd, ".loghorn"); got != want {
		t.Fatalf("logDir() = %q, want %q", got, want)
	}
}

func spanFixture(t *testing.T) [][]byte {
	t.Helper()
	b, err := os.ReadFile("testdata/web/spans.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Split(bytes.TrimSpace(b), []byte("\n"))
}

func TestPreloadStoredReadsLogsAndSpans(t *testing.T) {
	dir := t.TempDir()
	pino, err := os.ReadFile("testdata/web/pino.log")
	if err != nil {
		t.Fatal(err)
	}
	lines := spanFixture(t)
	join := func(ls [][]byte) []byte { return append(bytes.Join(ls, []byte("\n")), '\n') }
	files := map[string][]byte{
		"loghorn-2026-11-17.log":          pino,
		"loghorn-2026-11-17.traces.jsonl": join(lines[:4]),
		"loghorn.traces.jsonl":            join(lines[4:]),
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st := store.New(store.ModeLive, time.Now())
	preloadStored(st, dir, config.Input{})
	got := map[store.StreamCount]bool{}
	for _, c := range st.Streams() {
		got[c] = true
	}
	for _, w := range []store.StreamCount{{Name: "db", Source: "log", Count: 3}, {Name: "db", Source: "otel", Count: 2}, {Name: "db open", Source: "otel", Count: 1}, {Name: "job.run", Source: "otel", Count: 1}} {
		if !got[w] {
			t.Errorf("missing %v in %v", w, st.Streams())
		}
	}
	recs, _ := st.Snapshot()
	if id := recs[len(recs)-1].Fields["span_id"]; id != "a000000000000009" {
		t.Errorf("last record span_id = %v, want a000000000000009", id)
	}
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }

// main resolves its config with config.Load(cwd): the working directory
// anchors it, the same as .loghorn/ and the source-tree refusal, so all three
// agree on what "this project" means. This pins that call's contract —
// coverage of the walk itself belongs to package config.
func TestConfigForUsesWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, config.RelPath)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("[input]\nformat = \"json\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, path, err := config.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if path != p || cfg.Input.Format != config.FormatJSON {
		t.Fatalf("Load(%q) = %q, %q; want %q, json", dir, path, cfg.Input.Format, p)
	}
}

func TestWebStoreMode(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name                          string
		historical, ghMode, fileInput bool
		want                          store.Mode
		wantStart                     time.Time
	}{
		{"live", false, false, false, store.ModeLive, now},
		{"historical", true, false, false, store.ModeHistorical, time.Time{}},
		{"gh", false, true, false, store.ModeGHJob, time.Time{}},
		{"stdin from file", false, false, true, store.ModeHistorical, time.Time{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mode, start := webStoreMode(c.historical, c.ghMode, c.fileInput, now)
			if mode != c.want || !start.Equal(c.wantStart) {
				t.Fatalf("webStoreMode = %v, %v; want %v, %v", mode, start, c.want, c.wantStart)
			}
		})
	}
}
