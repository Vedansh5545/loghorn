package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
	_ "time/tzdata" // America/Los_Angeles without relying on the host's zoneinfo

	"github.com/jimbeveridge/loghorn/internal/config"
)

func spanFixture(t *testing.T) [][]byte {
	t.Helper()
	b, err := os.ReadFile("testdata/web/spans.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Split(bytes.TrimSpace(b), []byte("\n"))
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
