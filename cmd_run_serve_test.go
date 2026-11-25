package main

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/sock"
)

// freePort returns a loopback port that was free a moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// userPorts writes this environment's user config with the given [serve]
// ports, so a serve started by run -serve stays off 4380 and 4318.
func (e serveEnv) userPorts(t *testing.T, web, otlp int) {
	t.Helper()
	p := filepath.Join(e.home, ".config", "loghorn", "config.toml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[serve]\nweb-port = " + strconv.Itoa(web) + "\notlp-http-port = " + strconv.Itoa(otlp) + "\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunServeOneShot(t *testing.T) {
	e := newServeEnv(t)
	e.userPorts(t, freePort(t), freePort(t))
	cmd := e.cmd(t, context.Background(), "run", "-serve", "--", "sh", "-c",
		`echo '{"labels":{"schema":"app"},"message":"one-shot"}'; exit 3`)
	cmd.Env = append(cmd.Env, "XDG_CONFIG_HOME=")
	cmd.Stdin = ttyStdin(t) // waiting for Ctrl-C needs a terminal
	out := newWatchWriter("Ctrl-C to stop")
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	select {
	case <-out.seen:
	case <-time.After(10 * time.Second):
		t.Fatalf("no serving line; stderr %q", out.String())
	}
	if _, err := sock.Probe(e.socket); err != nil {
		t.Fatalf("the one-shot serve doesn't answer: %v", err)
	}
	e.waitLog(t, `"message":"one-shot"`)

	cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run -serve did not stop on SIGTERM")
	}
	if code := cmd.ProcessState.ExitCode(); code != 3 {
		t.Fatalf("exit %d; want the command's 3; stderr %q", code, out.String())
	}
	if _, err := os.Lstat(e.socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket left behind: %v", err)
	}
}

func TestRunServeUsesRunningServe(t *testing.T) {
	e := newServeEnv(t)
	e.startServe(t)
	r := e.run(t, nil, nil, "run", "-serve", "--", "echo", "hi")
	if r.code != 0 || !strings.Contains(r.stderr, "recording to serve as") || strings.Contains(r.stderr, "until Ctrl-C") {
		t.Fatalf("code %d stderr %q", r.code, r.stderr)
	}
}

func TestRunServeStartFailureFallsBack(t *testing.T) {
	e := newServeEnv(t)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	e.userPorts(t, freePort(t), busy.Addr().(*net.TCPAddr).Port)
	r := e.run(t, nil, []string{"XDG_CONFIG_HOME="}, "run", "-serve", "-serve-down", "skip", "--", "echo", "hi")
	if r.code != 0 || r.stdout != "hi\n" || !strings.Contains(r.stderr, "can't start serve") || !strings.Contains(r.stderr, "is in use") {
		t.Fatalf("code %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
}

// waitExit waits up to 10 s for cmd to exit after a signal and checks that the
// one-shot serve's socket is gone.
func (e serveEnv) waitExit(t *testing.T, cmd *exec.Cmd, stderr *watchWriter) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("run -serve kept running after the signal; stderr %q", stderr.String())
	}
	if _, err := os.Lstat(e.socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket left behind: %v", err)
	}
}

func TestRunServeTermDuringCommandStops(t *testing.T) {
	e := newServeEnv(t)
	e.userPorts(t, freePort(t), freePort(t))
	cmd := e.cmd(t, context.Background(), "run", "-serve", "--", "sh", "-c", "echo started; sleep 30")
	cmd.Env = append(cmd.Env, "XDG_CONFIG_HOME=")
	errw := newWatchWriter("recording to serve")
	cmd.Stderr = errw
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	<-errw.seen
	e.waitLog(t, "started")
	cmd.Process.Signal(syscall.SIGTERM) // forwarded to the command; run must not linger
	e.waitExit(t, cmd, errw)
	if strings.Contains(errw.String(), "until Ctrl-C") {
		t.Fatalf("run kept serving after SIGTERM: %q", errw.String())
	}
}

func TestRunServePipeSignalStops(t *testing.T) {
	e := newServeEnv(t)
	e.userPorts(t, freePort(t), freePort(t))
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pw.Close()
	cmd := e.cmd(t, context.Background(), "run", "-serve")
	cmd.Env = append(cmd.Env, "XDG_CONFIG_HOME=")
	cmd.Stdin = pr
	errw := newWatchWriter("recording to serve")
	cmd.Stderr = errw
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pr.Close()
	defer cmd.Process.Kill()
	<-errw.seen
	pw.WriteString("piped\n")
	e.waitLog(t, "piped")
	cmd.Process.Signal(syscall.SIGINT)
	e.waitExit(t, cmd, errw)
}

// One Ctrl-C ends both the command and the one-shot serve: the serve was up
// (and recording) while the command ran.
func TestRunServeOneCtrlCStopsAll(t *testing.T) {
	e := newServeEnv(t)
	e.userPorts(t, freePort(t), freePort(t))
	cmd := e.cmd(t, context.Background(), "run", "-serve", "-shutdown-grace", "300ms", "--", "sh", "-c", "echo started; sleep 30")
	cmd.Env = append(cmd.Env, "XDG_CONFIG_HOME=")
	cmd.Stdin = ttyStdin(t)
	errw := newWatchWriter("recording to serve")
	cmd.Stderr = errw
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	<-errw.seen
	e.waitLog(t, "started")
	if _, err := sock.Probe(e.socket); err != nil {
		t.Fatalf("serve isn't up while the command runs: %v", err)
	}
	cmd.Process.Signal(syscall.SIGINT) // Ctrl-C (the shell child ignores it here; the grace kills it)
	e.waitExit(t, cmd, errw)
	if strings.Contains(errw.String(), "Ctrl-C to stop") {
		t.Fatalf("run kept serving after Ctrl-C: %q", errw.String())
	}
}
