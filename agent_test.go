package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/jimbeveridge/loghorn/internal/sock"
)

func TestAgentEnv(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want string
	}{
		{nil, ""},
		{map[string]string{"CLAUDECODE": "1"}, "CLAUDECODE"},
		{map[string]string{"AI_AGENT": "claude-code"}, "AI_AGENT"},
		{map[string]string{"CODEX_SANDBOX": "seatbelt"}, "CODEX_SANDBOX"},
		{map[string]string{"CODEX_SANDBOX_NETWORK_DISABLED": "1"}, "CODEX_SANDBOX_NETWORK_DISABLED"},
		{map[string]string{"CLAUDECODE": ""}, ""},
		{map[string]string{"CLAUDECODE": "1", "LOGHORN_ALLOW_AGENT": "1"}, ""},
	} {
		if got := agentEnv(func(k string) string { return c.env[k] }); got != c.want {
			t.Errorf("agentEnv(%v) = %q; want %q", c.env, got, c.want)
		}
	}
}

func TestServeRefusesAgent(t *testing.T) {
	e := newServeEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := e.cmd(t, ctx, "serve", "-web-port", "0", "-otlp-http-port", "0")
	cmd.Env = append(cmd.Env, "CLAUDECODE=1")
	out, _ := cmd.CombinedOutput()
	if cmd.ProcessState.ExitCode() != 2 || !strings.Contains(string(out), "CLAUDECODE") || !strings.Contains(string(out), "LOGHORN_ALLOW_AGENT") || !strings.Contains(string(out), "left running after the agent") {
		t.Fatalf("exit %d; output %s", cmd.ProcessState.ExitCode(), out)
	}
	if _, err := os.Lstat(e.socket); err == nil {
		t.Fatal("serve bound its socket")
	}
}

func TestServeAllowedAgent(t *testing.T) {
	e := newServeEnv(t)
	cmd := e.cmd(t, context.Background(), "serve", "-web-port", "0", "-otlp-http-port", "0")
	cmd.Env = append(cmd.Env, "CLAUDECODE=1", "LOGHORN_ALLOW_AGENT=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := sock.Probe(e.socket); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("serve with LOGHORN_ALLOW_AGENT=1 did not start")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRunServeUnderAgentIsIgnored(t *testing.T) {
	e := newServeEnv(t)
	e.userPorts(t, freePort(t), freePort(t))
	r := e.run(t, nil, []string{"XDG_CONFIG_HOME=", "CLAUDECODE=1"}, "run", "-serve", "--", "echo", "hi")
	if r.code != 0 || r.stdout != "hi\n" || !strings.Contains(r.stderr, "ignoring -serve") || !strings.Contains(r.stderr, "is meant to run all the time") || !strings.Contains(r.stderr, "CLAUDECODE") {
		t.Fatalf("code %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	if len(e.incomingDirs(t)) != 1 {
		t.Fatalf("the run wasn't recorded to incoming: %v", e.incomingDirs(t))
	}
}

func TestRunServeNoTerminalDoesNotWait(t *testing.T) {
	e := newServeEnv(t)
	e.userPorts(t, freePort(t), freePort(t))
	r := e.run(t, nil, []string{"XDG_CONFIG_HOME="}, "run", "-serve", "--", "sh", "-c",
		`echo '{"labels":{"schema":"app"},"message":"no-tty"}'; exit 4`)
	if r.code != 4 || !strings.Contains(r.stderr, "no terminal") {
		t.Fatalf("code %d stderr %q", r.code, r.stderr)
	}
	if !strings.Contains(readFile(t, e.data+"/loghorn.log"), `"message":"no-tty"`) {
		t.Fatal("the run was not recorded")
	}
	if _, err := os.Lstat(e.socket); err == nil {
		t.Fatal("serve still has its socket")
	}
}

// ttyStdin gives a command a terminal on stdin.
func ttyStdin(t *testing.T) *os.File {
	t.Helper()
	ptmx, pts, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ptmx.Close(); pts.Close() })
	return pts
}
