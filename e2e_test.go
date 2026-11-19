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
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// These tests run the built binary. Each one works in its own t.TempDir(),
// which is outside the loghorn source tree (so the log file can be opened)
// and never the repository's own .loghorn, with HOME and XDG_CONFIG_HOME
// pointing at fresh temp dirs so the developer's config can't change framing.

var (
	buildOnce sync.Once
	buildDir  string
	buildPath string
	buildErr  error
)

func TestMain(m *testing.M) {
	// The test binary doubles as a helper child for the -otel tests.
	if h := os.Getenv("LOGHORN_TEST_HELPER"); h != "" {
		os.Exit(runHelper(h))
	}
	code := m.Run()
	if buildDir != "" {
		os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

// buildLoghorn builds the binary once per test binary and returns its path.
func buildLoghorn(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		buildDir, buildErr = os.MkdirTemp("", "loghorn-e2e-")
		if buildErr != nil {
			return
		}
		buildPath = filepath.Join(buildDir, "loghorn")
		out, err := exec.Command("go", "build", "-o", buildPath, ".").CombinedOutput()
		if err != nil {
			buildErr = errors.New(err.Error() + "\n" + string(out))
		}
	})
	if buildErr != nil {
		t.Fatalf("building loghorn: %v", buildErr)
	}
	return buildPath
}

// loghornCmd returns the binary's command, run in dir with a fresh HOME and
// XDG_CONFIG_HOME (appended after os.Environ(), so they win).
func loghornCmd(t *testing.T, ctx context.Context, dir string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(ctx, buildLoghorn(t), args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir(), "XDG_CONFIG_HOME="+t.TempDir())
	return cmd
}

type result struct {
	stdout, stderr string
	code           int
}

// runLoghorn runs the binary in dir to completion (at most 20 s) with stdin
// (nil = /dev/null).
func runLoghorn(t *testing.T, dir string, stdin io.Reader, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := loghornCmd(t, ctx, dir, args...)
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

// readLog returns dir's .loghorn/loghorn.log.
func readLog(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, ".loghorn", "loghorn.log"))
	if err != nil {
		t.Fatalf("reading the log file: %v", err)
	}
	return string(b)
}

// watchWriter collects writes and closes seen once they contain want.
type watchWriter struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	want string
	seen chan struct{}
	once sync.Once
}

func newWatchWriter(want string) *watchWriter {
	return &watchWriter{want: want, seen: make(chan struct{})}
}

func (w *watchWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if strings.Contains(w.buf.String(), w.want) {
		w.once.Do(func() { close(w.seen) })
	}
	return len(p), nil
}

func (w *watchWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// startUntil starts cmd with stdout going to a watchWriter and waits for want
// on it. done is closed when the process has been waited for.
func startUntil(t *testing.T, cmd *exec.Cmd, want string) (out *watchWriter, done chan struct{}) {
	t.Helper()
	out = newWatchWriter(want)
	cmd.Stdout = out
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done = make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-out.seen:
	case <-done:
		t.Fatalf("loghorn exited before %q; stdout %q", want, out.String())
	case <-time.After(10 * time.Second):
		t.Fatalf("no %q within 10 s; stdout %q", want, out.String())
	}
	return out, done
}

func TestWrapperPassThroughAndRecord(t *testing.T) {
	dir := t.TempDir()
	r := runLoghorn(t, dir, nil, "--", "sh", "-c", `printf 'a\033[31mred\033[0m\nplain\n'; echo err >&2; exit 3`)
	if r.code != 3 {
		t.Errorf("code = %d, want 3; stderr %q", r.code, r.stderr)
	}
	if want := "a\x1b[31mred\x1b[0m\nplain\n"; r.stdout != want {
		t.Errorf("stdout = %q, want %q", r.stdout, want)
	}
	if !strings.Contains(r.stderr, "err") || strings.Contains(r.stderr, "loghorn:") {
		t.Errorf("stderr = %q, want the child's err and no loghorn: line", r.stderr)
	}
	log := readLog(t, dir)
	for _, want := range []string{"ared\n", "plain\n", "err\n"} {
		if !strings.Contains(log, want) {
			t.Errorf("log file missing %q:\n%q", want, log)
		}
	}
	if strings.Contains(log, "\x1b") {
		t.Errorf("log file kept an escape sequence: %q", log)
	}
}

func TestWrapperPTY(t *testing.T) {
	dir := t.TempDir()
	ptmx, pts, err := pty.Open()
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	defer ptmx.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := loghornCmd(t, ctx, dir, "--", "sh", "-c", "test -t 1 && echo out-tty; test -t 2 && echo err-tty >&2")
	cmd.Stdout, cmd.Stderr = pts, pts
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pts.Close()

	var mu sync.Mutex
	var got bytes.Buffer
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			mu.Lock()
			got.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	err = cmd.Wait()
	if code := cmd.ProcessState.ExitCode(); code != 0 {
		t.Errorf("code = %d (%v), want 0", code, err)
	}
	// The master may still hold output written just before the exit.
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		s := got.String()
		mu.Unlock()
		if strings.Contains(s, "out-tty") && strings.Contains(s, "err-tty") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pty output = %q, want out-tty and err-tty", s)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWrapperSIGTERMEscalates(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := loghornCmd(t, ctx, dir, "-shutdown-grace", "200ms", "--", "sh", "-c", "trap '' TERM; echo ready; while :; do sleep 0.05; done")
	_, done := startUntil(t, cmd, "ready")
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loghorn did not exit within 2 s of SIGTERM")
	}
	if code := cmd.ProcessState.ExitCode(); code != 137 {
		t.Errorf("code = %d, want 137", code)
	}
}

func TestWrapperIgnoresSIGINTToItself(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := loghornCmd(t, ctx, dir, "--", "sh", "-c", "echo ready; sleep 1; echo done")
	out, done := startUntil(t, cmd, "ready")
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		t.Fatalf("loghorn exited on SIGINT to itself; stdout %q", out.String())
	case <-time.After(300 * time.Millisecond):
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("loghorn did not exit after the child finished")
	}
	if code := cmd.ProcessState.ExitCode(); code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "done") {
		t.Errorf("stdout = %q, want done", out.String())
	}
}

func TestWrapperFilter(t *testing.T) {
	dir := t.TempDir()
	r := runLoghorn(t, dir, nil, "-filter", "--", "sh", "-c",
		`echo "{\"severity\":\"INFO\",\"message\":\"i\"}"; echo "{\"severity\":\"ERROR\",\"message\":\"e\"}"; exit 2`)
	if r.code != 2 {
		t.Errorf("code = %d, want 2; stderr %q", r.code, r.stderr)
	}
	errLine := `{"severity":"ERROR","message":"e"}`
	if r.stdout != errLine+"\n" {
		t.Errorf("stdout = %q, want only the ERROR line", r.stdout)
	}
	log := readLog(t, dir)
	if !strings.Contains(log, `{"severity":"INFO","message":"i"}`) || !strings.Contains(log, errLine) {
		t.Errorf("log file should hold both lines:\n%s", log)
	}
}

func TestPipedStdinPassesThrough(t *testing.T) {
	dir := t.TempDir()
	r := runLoghorn(t, dir, strings.NewReader("one\ntwo\n"))
	if r.code != 0 || r.stdout != "one\ntwo\n" {
		t.Errorf("code %d stdout %q, want 0 and the input; stderr %q", r.code, r.stdout, r.stderr)
	}
	log := readLog(t, dir)
	if !strings.Contains(log, "one\n") || !strings.Contains(log, "two\n") {
		t.Errorf("log file should hold both lines:\n%s", log)
	}
}

func TestReplaysNeedWebOrFilter(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(t.TempDir(), "in.log")
	if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	devNull := func() *os.File {
		d, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.Close() })
		return d
	}

	for _, tc := range []struct {
		name  string
		stdin io.Reader
		args  []string
		want  string
	}{
		{"file", f, nil, "loghorn: reading a file (loghorn < file) needs -web or -filter"},
		{"historical", devNull(), []string{"-historical"}, "loghorn: -historical needs -web or -filter"},
		{"nothing", devNull(), nil, "loghorn: nothing to read: give a command after --, pipe input into loghorn, or pass -web to browse the stored logs"},
	} {
		r := runLoghorn(t, dir, tc.stdin, tc.args...)
		if r.code != 2 || !strings.Contains(r.stderr, tc.want) {
			t.Errorf("%s: code %d stderr %q, want 2 and %q", tc.name, r.code, r.stderr, tc.want)
		}
	}
}

func TestRemovedFlagsExit2(t *testing.T) {
	r := runLoghorn(t, t.TempDir(), nil, "-theme", "dark", "--", "true")
	if r.code != 2 || !strings.Contains(r.stderr, "loghorn: -theme was removed along with the terminal UI") {
		t.Errorf("code %d stderr %q", r.code, r.stderr)
	}
}

func TestFileWithFilter(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(t.TempDir(), "in.log")
	errLine := `{"severity":"ERROR","message":"e"}`
	if err := os.WriteFile(p, []byte(errLine+"\n"+`{"severity":"INFO","message":"i"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r := runLoghorn(t, dir, f, "-filter")
	if r.code != 0 || r.stdout != errLine+"\n" {
		t.Errorf("code %d stdout %q, want 0 and the ERROR line; stderr %q", r.code, r.stdout, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, ".loghorn")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("reading a file created .loghorn (stat err %v)", err)
	}
}

func TestWebWithFilterAllowed(t *testing.T) {
	r := runLoghorn(t, t.TempDir(), nil, "-web", "-filter", "--", "sh", "-c", "echo hi")
	if r.code != 0 || !strings.Contains(r.stderr, "web view at http://127.0.0.1:") {
		t.Errorf("code %d stderr %q", r.code, r.stderr)
	}
}

func TestMissingCommand(t *testing.T) {
	r := runLoghorn(t, t.TempDir(), nil, "--", "no-such-binary-xyz")
	if r.code != 127 || !strings.HasPrefix(r.stderr, "loghorn:") {
		t.Errorf("code %d stderr %q, want 127 and a loghorn: message", r.code, r.stderr)
	}
}

func TestDisplayWarns(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".config", "loghorn", "config.toml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("[display]\nformat-sql = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := runLoghorn(t, dir, nil, "--", "true")
	if r.code != 0 || !strings.Contains(r.stderr, "[display] is ignored") {
		t.Errorf("code %d stderr %q", r.code, r.stderr)
	}
}
