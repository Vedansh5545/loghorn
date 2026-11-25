package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// cliRun runs the built binary to completion in dir with the given env and
// stdin (nil = /dev/null).
func cliRun(t *testing.T, dir string, env []string, stdin io.Reader, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, buildLoghorn(t), args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("running loghorn %q: %v", args, err)
	}
	if ctx.Err() != nil {
		t.Fatalf("loghorn %q timed out; stderr:\n%s", args, stderr.String())
	}
	return result{stdout: stdout.String(), stderr: stderr.String(), code: cmd.ProcessState.ExitCode()}
}

func TestCLIQueryE2E(t *testing.T) {
	// A short TMPDIR keeps <TMPDIR>/loghorn/<pid>.sock inside sun_path.
	tmp, err := os.MkdirTemp("", "lhq")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	env := []string{"HOME=" + t.TempDir(), "TMPDIR=" + tmp}
	// Remove inherited variables that would move the config or socket dirs.
	env = append(env, "XDG_CONFIG_HOME="+t.TempDir(), "XDG_RUNTIME_DIR="+t.TempDir())
	serverDir, clientDir := t.TempDir(), t.TempDir()

	// The server shows a file in open's view.
	fixture, err := filepath.Abs("testdata/web/app.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := exec.CommandContext(ctx, buildLoghorn(t), "open", fixture)
	srv.Dir = serverDir
	srv.Env = append(os.Environ(), env...)
	srvErr := newWatchWriter("web view at http://127.0.0.1:")
	srv.Stderr = srvErr
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = srv.Wait(); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-srvErr.seen:
	case <-done:
		t.Fatalf("server exited early; stderr %q", srvErr.String())
	case <-time.After(10 * time.Second):
		t.Fatalf("no web URL within 10 s; stderr %q", srvErr.String())
	}
	m := regexp.MustCompile(`web view at (http://127\.0\.0\.1:\d+)/`).FindStringSubmatch(srvErr.String())
	if m == nil {
		t.Fatalf("no URL in %q", srvErr.String())
	}
	base := m[1]
	q := `/?q=` + `%7B%22stream%22%3A%22app%22%2C%22range%22%3A%7B%22from%22%3A%22all%22%7D%7D`

	// Positional URL, stdin /dev/null: TSV with a header and 3 rows.
	r := cliRun(t, clientDir, env, nil, base+q) // the shortcut for query
	if r.code != 0 {
		t.Fatalf("positional: exit %d: %s", r.code, r.stderr)
	}
	lines := strings.Split(strings.TrimRight(r.stdout, "\n"), "\n")
	if len(lines) != 4 || !strings.Contains(lines[0], "\t") {
		t.Fatalf("positional stdout = %q; want a TSV header and 3 rows", r.stdout)
	}
	want := r.stdout

	// Stdin a pipe with junk: ignored, same output, nothing recorded.
	r = cliRun(t, clientDir, env, strings.NewReader("junk line\nmore junk\n"), base+q)
	if r.code != 0 || r.stdout != want {
		t.Fatalf("piped stdin: exit %d, stdout %q, stderr %q; want %q", r.code, r.stdout, r.stderr, want)
	}
	if _, err := os.Stat(filepath.Join(clientDir, ".loghorn")); !os.IsNotExist(err) {
		t.Errorf("query mode created .loghorn in its directory (stat err %v)", err)
	}

	// @file.
	urlFile := filepath.Join(clientDir, "url.txt")
	if err := os.WriteFile(urlFile, []byte(base+q+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = cliRun(t, clientDir, env, nil, "query", "@"+urlFile)
	if r.code != 0 || r.stdout != want {
		t.Fatalf("@file: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}

	// @- reads the URL from stdin.
	r = cliRun(t, clientDir, env, strings.NewReader(base+q+"\n"), "query", "@-")
	if r.code != 0 || r.stdout != want {
		t.Fatalf("-url @-: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}

	// -o out.yaml picks YAML by extension.
	out := filepath.Join(clientDir, "out.yaml")
	r = cliRun(t, clientDir, env, nil, "query", base+q, "-o", out)
	if r.code != 0 {
		t.Fatalf("-o: exit %d: %s", r.code, r.stderr)
	}
	b, err := os.ReadFile(out)
	if err != nil || !strings.HasPrefix(string(b), "view:") {
		t.Fatalf("out.yaml = %q (err %v); want it to start with view:", b, err)
	}

	// -to-sql prints a SELECT.
	r = cliRun(t, clientDir, env, nil, "query", "-to-sql", base+q)
	if r.code != 0 || !strings.Contains(r.stdout, "SELECT") {
		t.Fatalf("-to-sql: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}

	if err := srv.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("server did not exit on SIGINT")
	}
}

// A host-less URL goes to serve through its socket: records a run sent to
// serve come back as the query's rows.
func TestQueryHostlessThroughServe(t *testing.T) {
	e := newServeEnv(t)
	e.startServe(t)
	f, err := os.Open("testdata/web/app.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if r := e.run(t, f, nil, "run"); r.code != 0 {
		t.Fatalf("run: exit %d %s", r.code, r.stderr)
	}
	// Records a run sends carry its service; "*" asks for every service, as
	// the page's All does.
	q := "/?q=" + url.QueryEscape(`{"stream":"app","service":"*","range":{"from":"all"}}`)
	deadline := time.Now().Add(5 * time.Second)
	for {
		r := e.run(t, nil, nil, "query", q)
		if r.code == 0 && len(strings.Split(strings.TrimRight(r.stdout, "\n"), "\n")) == 4 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("query: exit %d stdout %q stderr %q; want a header and 3 rows", r.code, r.stdout, r.stderr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
