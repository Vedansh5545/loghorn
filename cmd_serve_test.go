package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/sock"
)

// serveEnv is an isolated environment for a serve and its clients: HOME,
// data folder and a socket path short enough for sun_path.
type serveEnv struct {
	home, data, socket string
}

func newServeEnv(t *testing.T) serveEnv {
	t.Helper()
	sdir, err := os.MkdirTemp("/tmp", "lh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sdir) })
	return serveEnv{home: t.TempDir(), data: t.TempDir(), socket: filepath.Join(sdir, "s.sock")}
}

// environ is os.Environ() without the AI-agent markers (the tests themselves
// may run under one), with this env's HOME, LOGHORN_DATA_DIR and
// LOGHORN_SOCKET appended, so they win.
func (e serveEnv) environ() []string {
	return append(append(os.Environ(), noAgent...), "HOME="+e.home, "LOGHORN_DATA_DIR="+e.data, "LOGHORN_SOCKET="+e.socket)
}

// cmd returns the binary's command in this environment.
func (e serveEnv) cmd(t *testing.T, ctx context.Context, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(ctx, buildLoghorn(t), args...)
	cmd.Env = e.environ()
	cmd.Dir = t.TempDir()
	return cmd
}

// startServe runs `loghorn serve` on ephemeral ports until the test ends and
// waits until its socket answers.
func (e serveEnv) startServe(t *testing.T, extra ...string) (*exec.Cmd, *lockedBuf) {
	t.Helper()
	args := append([]string{"serve", "-web-port", "0", "-otlp-http-port", "0"}, extra...)
	cmd := e.cmd(t, context.Background(), args...)
	stderr := &lockedBuf{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGTERM)
		cmd.Wait()
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := sock.Probe(e.socket); err == nil {
			return cmd, stderr
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve did not answer on %s; stderr:\n%s", e.socket, stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// lockedBuf is a bytes.Buffer safe to read while a process writes to it.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestServeCommand(t *testing.T) {
	e := newServeEnv(t)
	cmd, stderr := e.startServe(t)
	if !strings.Contains(stderr.String(), "loghorn serve: web http://127.0.0.1:") {
		t.Errorf("stderr = %q", stderr.String())
	}
	cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve exited with %v; stderr:\n%s", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop on SIGTERM")
	}
	if _, err := os.Lstat(e.socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket left behind: %v", err)
	}
}

func TestServePortInUseMessage(t *testing.T) {
	e := newServeEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := e.cmd(t, ctx, "serve", "-web-port", "0", "-otlp-http-port", port)
	out, _ := cmd.CombinedOutput()
	if cmd.ProcessState.ExitCode() != 1 {
		t.Fatalf("exit %d; output:\n%s", cmd.ProcessState.ExitCode(), out)
	}
	for _, want := range []string{"127.0.0.1:" + port + " is in use", "-otlp-http-port", "[serve] otlp-http-port"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestServeBadConfigExit2(t *testing.T) {
	e := newServeEnv(t)
	p := filepath.Join(e.home, ".config", "loghorn", "config.toml")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("[run]\nserve-down = \"x\"\n"), 0o644)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := e.cmd(t, ctx, "serve")
	cmd.Env = append(cmd.Env, "XDG_CONFIG_HOME=")
	out, _ := cmd.CombinedOutput()
	if cmd.ProcessState.ExitCode() != 2 || !strings.Contains(string(out), "serve-down") {
		t.Fatalf("exit %d; output:\n%s", cmd.ProcessState.ExitCode(), out)
	}
}

func TestRunPassesChildFlags(t *testing.T) {
	r := runLoghorn(t, t.TempDir(), nil, "run", "sh", "-c", "echo $0", "-filter")
	if r.code != 0 || r.stdout != "-filter\n" {
		t.Fatalf("code %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
}

func TestShortcuts(t *testing.T) {
	r := runLoghorn(t, t.TempDir(), nil, "--", "echo", "hi")
	if r.code != 0 || r.stdout != "hi\n" {
		t.Fatalf("-- shortcut: code %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	r = runLoghorn(t, t.TempDir(), nil, "http://127.0.0.1:1/?q=%7B%7D")
	if r.code != 1 {
		t.Fatalf("URL shortcut: code %d stderr %q; want 1 (connection refused)", r.code, r.stderr)
	}
}
