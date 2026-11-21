package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoWith makes a temp git repo whose .config/loghorn holds the named files
// (a name ending in / is a directory); none at all means no .config.
func repoWith(t *testing.T, names ...string) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(repo, ".config", "loghorn")
	for _, n := range names {
		p := filepath.Join(dir, n)
		var err error
		if strings.HasSuffix(n, "/") {
			err = os.MkdirAll(p, 0o755)
		} else if err = os.MkdirAll(dir, 0o755); err == nil {
			err = os.WriteFile(p, []byte("# x\n"), 0o644)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

func TestRepoConfigNotice(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	base := filepath.Join(t.TempDir(), "loghorn") // not yet created
	repo := repoWith(t, "queries.toml")
	sub := filepath.Join(repo, "pkg")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got := repoConfigNotice(sub, base)
	cfg := filepath.Join(repo, ".config", "loghorn")
	if !strings.HasPrefix(got, "loghorn: ") || !strings.Contains(got, filepath.Join(base, "services")) ||
		!strings.Contains(got, cfg) {
		t.Fatalf("notice = %q", got)
	}
	if again := repoConfigNotice(repo, base); again != "" {
		t.Fatalf("second notice = %q", again)
	}
	data, err := os.ReadFile(filepath.Join(base, "notified-repos"))
	if err != nil || string(data) != cfg+"\n" {
		t.Fatalf("notified-repos: %v %q", err, data)
	}
	// The repo's own files are untouched.
	if b, err := os.ReadFile(filepath.Join(cfg, "queries.toml")); err != nil || string(b) != "# x\n" {
		t.Fatalf("repo file changed: %v %q", err, b)
	}

	// A second repo with schemas/ is noticed too and appended.
	repo2 := repoWith(t, "schemas/")
	if got := repoConfigNotice(repo2, base); got == "" {
		t.Fatal("schemas/ repo not noticed")
	}
	data, _ = os.ReadFile(filepath.Join(base, "notified-repos"))
	if strings.Count(string(data), "\n") != 2 {
		t.Fatalf("notified-repos = %q", data)
	}

	if got := repoConfigNotice(repoWith(t, "config.toml"), base); got != "" {
		t.Fatalf("config.toml only: %q", got)
	}
	if got := repoConfigNotice(repoWith(t), base); got != "" {
		t.Fatalf("no .config: %q", got)
	}
}

func TestRepoConfigNoticeSwallowsErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// The base is a file, so notified-repos cannot be written.
	base := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(base, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := repoConfigNotice(repoWith(t, "queries.toml"), base); got != "" {
		t.Fatalf("notice with unwritable base = %q", got)
	}
}
