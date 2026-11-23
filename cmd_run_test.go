package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// run runs the binary to completion (at most 30 s) in this environment, in a
// fresh directory, with stdin (nil = /dev/null) and extra environment.
func (e serveEnv) run(t *testing.T, stdin io.Reader, extraEnv []string, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := e.cmd(t, ctx, args...)
	cmd.Env = append(cmd.Env, extraEnv...)
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

// readFile returns a file's contents, or "" when it doesn't exist.
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(b)
}

// waitFile waits up to 5 s for path to contain every want, and returns it.
func waitFile(t *testing.T, path string, want ...string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s := readFile(t, path)
		ok := true
		for _, w := range want {
			ok = ok && strings.Contains(s, w)
		}
		if ok {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s lacks some of %q:\n%s", path, want, s)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitLog waits for serve's loghorn.log to contain every want.
func (e serveEnv) waitLog(t *testing.T, want ...string) string {
	t.Helper()
	return waitFile(t, filepath.Join(e.data, "loghorn.log"), want...)
}

func TestRunRecordsToServe(t *testing.T) {
	e := newServeEnv(t)
	e.startServe(t)
	errLine := `{"labels":{"schema":"app"},"severity":"ERROR","message":"x"}`
	r := e.run(t, nil, nil, "run", "--", "sh", "-c", "echo '"+errLine+"'; echo plain; exit 3")
	if r.code != 3 || r.stdout != errLine+"\nplain\n" {
		t.Fatalf("code %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stderr, "recording to serve as") || !strings.Contains(r.stderr, "web view at http://127.0.0.1:") {
		t.Fatalf("stderr = %q", r.stderr)
	}
	e.waitLog(t, errLine+"\n", "plain\n")
}

func TestRunChildOTLPReachesServe(t *testing.T) {
	e := newServeEnv(t)
	e.startServe(t)
	r := e.run(t, nil, []string{"LOGHORN_TEST_HELPER=otel-child", "OTEL_SERVICE_NAME=e2e"}, "run", "--", os.Args[0])
	if r.code != 3 {
		t.Fatalf("code %d stderr %q", r.code, r.stderr)
	}
	log := e.waitLog(t, "hello otel", "plain before", "plain after", `"message":"before"`)
	waitFile(t, filepath.Join(e.data, "loghorn.traces.jsonl"), "e2e.span")
	time.Sleep(300 * time.Millisecond)
	if log = readFile(t, filepath.Join(e.data, "loghorn.log")); strings.Contains(log, `"message":"after"`) {
		t.Fatalf("a JSON stdout line was stored after the child's own OTLP arrived:\n%s", log)
	}
}

func TestRunPipeToServe(t *testing.T) {
	e := newServeEnv(t)
	e.startServe(t)
	p := filepath.Join(t.TempDir(), "in.log")
	os.WriteFile(p, []byte("one\ntwo\n"), 0o644)
	f, _ := os.Open(p)
	defer f.Close()
	r := e.run(t, f, nil, "run")
	if r.code != 0 || r.stdout != "one\ntwo\n" {
		t.Fatalf("code %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	e.waitLog(t, "one\n", "two\n")
}

func TestRunServeDiesMidRun(t *testing.T) {
	e := newServeEnv(t)
	srv, _ := e.startServe(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := e.cmd(t, ctx, "run", "--", "sh", "-c",
		`echo first; sleep 1; i=0; while [ $i -lt 2000 ]; do echo line$i; i=$((i+1)); done`)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	e.waitLog(t, "first\n")
	srv.Process.Signal(syscall.SIGKILL)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("run: %v; stderr %q", err, stderr.String())
	}
	if n := strings.Count(stdout.String(), "\n"); n != 2001 {
		t.Fatalf("stdout has %d lines; want 2001", n)
	}
	if c := strings.Count(stderr.String(), "serve stopped answering"); c != 1 {
		t.Fatalf("stderr warns %d times; want once:\n%s", c, stderr.String())
	}
}

func TestRunFilterStillLocal(t *testing.T) {
	e := newServeEnv(t)
	e.startServe(t)
	info := `{"severity":"INFO","message":"i"}`
	errLine := `{"severity":"ERROR","message":"e"}`
	r := e.run(t, nil, nil, "run", "-filter", "--", "sh", "-c", "echo '"+info+"'; echo '"+errLine+"'")
	if r.code != 0 || r.stdout != errLine+"\n" {
		t.Fatalf("code %d stdout %q", r.code, r.stdout)
	}
	e.waitLog(t, info, errLine)
}

func TestRunServeDownStillRunsChild(t *testing.T) {
	e := newServeEnv(t)
	r := e.run(t, nil, nil, "run", "--", "echo", "hi")
	if r.code != 0 || r.stdout != "hi\n" || !strings.Contains(r.stderr, "loghorn serve isn't running") {
		t.Fatalf("code %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
}
