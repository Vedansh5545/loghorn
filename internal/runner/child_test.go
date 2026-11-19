package runner

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

func sh(t *testing.T, script string, o Options) *Child {
	t.Helper()
	c, err := StartChild([]string{"sh", "-c", script}, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

func wait(t *testing.T, c *Child) {
	t.Helper()
	select {
	case <-c.Exited():
	case <-time.After(10 * time.Second):
		t.Fatal("child did not exit")
	}
}

func TestPipesKeepStreamsSeparate(t *testing.T) {
	c := sh(t, "echo out; echo err >&2", Options{})
	if got := readAll(t, c.Stdout()); got != "out\n" {
		t.Errorf("stdout = %q", got)
	}
	if got := readAll(t, c.Stderr()); got != "err\n" {
		t.Errorf("stderr = %q", got)
	}
}

func TestPipesAreNotATerminal(t *testing.T) {
	c := sh(t, "test -t 1 || echo notty1; test -t 2 || echo notty2 >&2", Options{})
	if got := readAll(t, c.Stdout()); got != "notty1\n" {
		t.Errorf("stdout = %q", got)
	}
	if got := readAll(t, c.Stderr()); got != "notty2\n" {
		t.Errorf("stderr = %q", got)
	}
}

func TestPTYIsATerminal(t *testing.T) {
	c := sh(t, "test -t 1 && echo tty1; test -t 2 && echo tty2 >&2", Options{PTY: true})
	if c.Stderr() != nil {
		t.Error("Stderr() should be nil in PTY mode")
	}
	got := readAll(t, c.Stdout()) // io.ReadAll fails the test on any non-EOF error
	if !strings.Contains(got, "tty1") || !strings.Contains(got, "tty2") {
		t.Errorf("output = %q", got)
	}
}

func TestStdinPassedDirectly(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	pw.WriteString("abc\n")
	pw.Close()
	c := sh(t, "read x; echo got-$x", Options{Stdin: pr})
	if got := readAll(t, c.Stdout()); got != "got-abc\n" {
		t.Errorf("stdout = %q", got)
	}
}

func TestSameProcessGroup(t *testing.T) {
	c := sh(t, "sleep 5", Options{})
	defer c.Signal(syscall.SIGKILL)
	pg, err := syscall.Getpgid(c.Pid())
	if err != nil {
		t.Fatal(err)
	}
	if pg != syscall.Getpgrp() {
		t.Errorf("child pgid %d, loghorn pgid %d", pg, syscall.Getpgrp())
	}
}

func TestEnvPassed(t *testing.T) {
	c := sh(t, "echo $LH_X", Options{Env: append(os.Environ(), "LH_X=1")})
	if got := readAll(t, c.Stdout()); got != "1\n" {
		t.Errorf("stdout = %q", got)
	}
}

func TestExitStatus(t *testing.T) {
	for script, want := range map[string]int{"exit 3": 3, "kill -KILL $$": 137, "kill -TERM $$": 143, "exit 0": 0} {
		c := sh(t, script, Options{})
		wait(t, c)
		if got := c.ExitStatus(); got != want {
			t.Errorf("%q: status %d, want %d", script, got, want)
		}
	}
}

func TestSignalReachesChild(t *testing.T) {
	c := sh(t, "trap 'echo got; exit 7' TERM; echo ready; while :; do sleep 0.05; done", Options{})
	br := bufio.NewReader(c.Stdout())
	if line, _ := br.ReadString('\n'); line != "ready\n" {
		t.Fatalf("first line %q", line)
	}
	if err := c.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, br); !strings.Contains(got, "got") {
		t.Errorf("output = %q", got)
	}
	wait(t, c)
	if c.ExitStatus() != 7 {
		t.Errorf("status %d", c.ExitStatus())
	}
	if err := c.Signal(syscall.SIGTERM); err != nil {
		t.Errorf("Signal after exit = %v", err)
	}
}

func TestInheritSize(t *testing.T) {
	m, s, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	defer s.Close()
	if err := pty.Setsize(m, &pty.Winsize{Rows: 40, Cols: 100}); err != nil {
		t.Fatal(err)
	}
	c := sh(t, "sleep 0.2; stty size <&1", Options{PTY: true})
	if err := c.InheritSize(s); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, c.Stdout()); !strings.Contains(got, "40 100") {
		t.Errorf("output = %q", got)
	}
}

func TestStartChildMissingBinary(t *testing.T) {
	_, err := StartChild([]string{"no-such-binary-xyz"}, Options{})
	if !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("err = %v", err)
	}
	if _, err := StartChild(nil, Options{}); err == nil {
		t.Error("nil argv: want error")
	}
}
