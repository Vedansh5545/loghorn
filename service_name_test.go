package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunService(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "proj")
	sub := filepath.Join(proj, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(proj, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(root, "wt")
	os.MkdirAll(wt, 0o755)
	os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: elsewhere\n"), 0o644)
	plain := filepath.Join(root, "plain")
	os.MkdirAll(plain, 0o755)

	for _, c := range []struct {
		name, flag string
		env        []string
		cwd        string
		args       []string
		want       string
	}{
		{"flag wins", "svc", []string{"OTEL_SERVICE_NAME=a"}, sub, []string{"npm"}, "svc"},
		{"last env entry", "", []string{"OTEL_SERVICE_NAME=a", "OTEL_SERVICE_NAME=b"}, sub, []string{"npm"}, "b"},
		{"repo dir and command", "", nil, sub, []string{"/usr/bin/npm", "test"}, "proj-npm"},
		{".git file counts", "", nil, wt, []string{"go"}, "wt-go"},
		{"no repo: cwd", "", nil, plain, []string{"make"}, "plain-make"},
		{"pipe: dir only", "", nil, sub, nil, "proj"},
	} {
		if got := runService(c.flag, c.env, c.cwd, c.args); got != c.want {
			t.Errorf("%s: runService = %q; want %q", c.name, got, c.want)
		}
	}
}
