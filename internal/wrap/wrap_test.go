package wrap

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/ingest"
	"github.com/jimbeveridge/loghorn/internal/recorder"
	"github.com/jimbeveridge/loghorn/internal/runner"
)

// sink collects every record the recorder writes to its log-file sink.
type sink struct {
	mu   sync.Mutex
	recs []string
}

func (s *sink) add(rec []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = append(s.recs, string(rec))
}

func (s *sink) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.recs)
}

// newRec returns a recorder whose log-file sink collects records.
func newRec(t *testing.T) (*recorder.Recorder, *sink) {
	t.Helper()
	s := &sink{}
	return recorder.New(recorder.Options{Sink: s.add, Input: config.Default().Input}), s
}

// lockedBuf is a bytes.Buffer safe to read while Run writes to it.
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

func start(t *testing.T, script string, pty bool) *runner.Child {
	t.Helper()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { devnull.Close() })
	c, err := runner.StartChild([]string{"sh", "-c", script}, runner.Options{PTY: pty, Stdin: devnull})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.Signal(syscall.SIGKILL)
		c.Close()
	})
	return c
}

// runAsync runs Run in a goroutine and returns a channel with its status.
func runAsync(c *runner.Child, o Options) <-chan int {
	ch := make(chan int, 1)
	go func() { ch <- Run(c, o) }()
	return ch
}

func waitStatus(t *testing.T, ch <-chan int, within time.Duration) int {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(within):
		t.Fatalf("Run did not return within %v", within)
		return -1
	}
}

func waitFor(t *testing.T, b *lockedBuf, s string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(b.String(), s) {
		if time.Now().After(deadline) {
			t.Fatalf("output never contained %q; got %q", s, b.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPassThroughBytesExact(t *testing.T) {
	script := `printf 'a\033[31mred\033[0m\n\377\376 raw\nno-newline'`
	want, err := exec(script)
	if err != nil {
		t.Fatal(err)
	}
	rec, s := newRec(t)
	var out, errb lockedBuf
	status := Run(start(t, script, false), Options{Recorder: rec, StripANSI: true, Stdout: &out, Stderr: &errb})
	if status != 0 {
		t.Fatalf("status %d", status)
	}
	if out.String() != want {
		t.Fatalf("stdout %q, want %q", out.String(), want)
	}
	if got, w := s.all(), []string{"ared", "\xff\xfe raw", "no-newline"}; !slices.Equal(got, w) {
		t.Fatalf("sink %q, want %q", got, w)
	}
}

func TestStdoutStderrSeparate(t *testing.T) {
	rec, s := newRec(t)
	var out, errb lockedBuf
	Run(start(t, `echo o; echo e >&2`, false), Options{Recorder: rec, Stdout: &out, Stderr: &errb})
	if out.String() != "o\n" || errb.String() != "e\n" {
		t.Fatalf("stdout %q stderr %q", out.String(), errb.String())
	}
	got := s.all()
	slices.Sort(got)
	if !slices.Equal(got, []string{"e", "o"}) {
		t.Fatalf("sink %q", got)
	}
}

func TestPTYPassThrough(t *testing.T) {
	rec, s := newRec(t)
	var out, errb lockedBuf
	Run(start(t, `printf 'x\ny\n'; echo e >&2`, true), Options{Recorder: rec, StripANSI: true, Stdout: &out, Stderr: &errb})
	got := strings.ReplaceAll(out.String(), "\r\n", "\n")
	if !strings.Contains(got, "x\ny\n") || !strings.Contains(got, "e\n") {
		t.Fatalf("stdout %q", out.String())
	}
	if errb.String() != "" {
		t.Fatalf("stderr %q, want empty under a pty", errb.String())
	}
	if recs := s.all(); !slices.Equal(recs, []string{"x", "y", "e"}) {
		t.Fatalf("sink %q", recs)
	}
}

func TestRecordingMatchesPlainFraming(t *testing.T) {
	path, err := filepath.Abs(filepath.Join("..", "..", "testdata", "gcloud-tail.json"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var want []string
	if err := ingest.Records(f, config.FormatAuto, func(rec []byte) { want = append(want, string(rec)) }); err != nil {
		t.Fatal(err)
	}

	rec, s := newRec(t)
	var out, errb lockedBuf
	Run(start(t, "cat '"+path+"'", false), Options{Recorder: rec, StripANSI: true, Stdout: &out, Stderr: &errb})
	if got := s.all(); !slices.Equal(got, want) {
		t.Fatalf("sink %d records, want %d:\n%q\nwant\n%q", len(got), len(want), got, want)
	}
}

func TestFilterWithChild(t *testing.T) {
	info := `{"severity":"INFO","textPayload":"routine"}`
	errLine := `{"severity":"ERROR","textPayload":"boom"}`
	script := "echo '" + info + "'; echo '" + errLine + "' >&2; sleep 0.1; echo 'panic: boom'"
	rec, s := newRec(t)
	var out, errb lockedBuf
	Run(start(t, script, false), Options{Recorder: rec, Filter: true, Stdout: &out, Stderr: &errb})
	if want := errLine + "\npanic: boom\n"; out.String() != want {
		t.Fatalf("stdout %q, want %q", out.String(), want)
	}
	if errb.String() != "" {
		t.Fatalf("stderr %q, want empty", errb.String())
	}
	got := s.all()
	slices.Sort(got)
	want := []string{errLine, info, "panic: boom"}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("sink %q, want %q", got, want)
	}
}

func TestExitStatusPassesThrough(t *testing.T) {
	rec, _ := newRec(t)
	var out, errb lockedBuf
	if got := Run(start(t, "exit 5", false), Options{Recorder: rec, Stdout: &out, Stderr: &errb}); got != 5 {
		t.Fatalf("status %d, want 5", got)
	}
}

func TestSIGINTDropped(t *testing.T) {
	rec, _ := newRec(t)
	var out, errb lockedBuf
	sigs := make(chan os.Signal, 1)
	ch := runAsync(start(t, "sleep 0.3; exit 0", false), Options{Recorder: rec, Stdout: &out, Stderr: &errb, Signals: sigs, Grace: 5 * time.Second})
	sigs <- syscall.SIGINT
	sigs <- syscall.SIGQUIT
	if got := waitStatus(t, ch, 5*time.Second); got != 0 {
		t.Fatalf("status %d, want 0", got)
	}
}

// The Review Focus case: loghorn handles SIGINT through signal.Notify, so the
// child starts with the default disposition, not an inherited SIG_IGN.
func TestChildGetsDefaultSIGINT(t *testing.T) {
	sigs, stop := NotifySignals()
	defer stop()
	rec, _ := newRec(t)
	var out, errb lockedBuf
	ch := runAsync(start(t, `kill -INT $$; sleep 2; exit 0`, false), Options{Recorder: rec, Stdout: &out, Stderr: &errb, Signals: sigs})
	if got := waitStatus(t, ch, time.Second); got != 130 {
		t.Fatalf("status %d, want 130", got)
	}
}

func TestSIGTERMForwarded(t *testing.T) {
	rec, _ := newRec(t)
	var out, errb lockedBuf
	sigs := make(chan os.Signal, 1)
	ch := runAsync(start(t, `trap 'echo got; exit 7' TERM; echo ready; while :; do sleep 0.05; done`, false),
		Options{Recorder: rec, Stdout: &out, Stderr: &errb, Signals: sigs, Grace: 5 * time.Second})
	waitFor(t, &out, "ready")
	sigs <- syscall.SIGTERM
	if got := waitStatus(t, ch, 5*time.Second); got != 7 {
		t.Fatalf("status %d, want 7", got)
	}
	if !strings.Contains(out.String(), "got") {
		t.Fatalf("stdout %q lacks the trap's output", out.String())
	}
}

func TestSIGTERMEscalates(t *testing.T) {
	rec, _ := newRec(t)
	var out, errb lockedBuf
	sigs := make(chan os.Signal, 1)
	ch := runAsync(start(t, `trap '' TERM; echo ready; while :; do sleep 0.05; done`, false),
		Options{Recorder: rec, Stdout: &out, Stderr: &errb, Signals: sigs, Grace: 200 * time.Millisecond})
	waitFor(t, &out, "ready")
	sent := time.Now()
	sigs <- syscall.SIGTERM
	got := waitStatus(t, ch, 3*time.Second)
	took := time.Since(sent)
	if got != 137 {
		t.Fatalf("status %d, want 137", got)
	}
	if took < 200*time.Millisecond || took > 1500*time.Millisecond {
		t.Fatalf("returned after %v, want 200ms-1.5s", took)
	}
}

func TestSIGHUPForwarded(t *testing.T) {
	rec, _ := newRec(t)
	var out, errb lockedBuf
	sigs := make(chan os.Signal, 1)
	ch := runAsync(start(t, `trap 'exit 9' HUP; echo ready; while :; do sleep 0.05; done`, false),
		Options{Recorder: rec, Stdout: &out, Stderr: &errb, Signals: sigs})
	waitFor(t, &out, "ready")
	sigs <- syscall.SIGHUP
	if got := waitStatus(t, ch, 5*time.Second); got != 9 {
		t.Fatalf("status %d, want 9", got)
	}
}

func TestSIGWINCHWithoutTerminal(t *testing.T) {
	rec, _ := newRec(t)
	var out, errb lockedBuf
	sigs := make(chan os.Signal, 1)
	ch := runAsync(start(t, `sleep 0.2; exit 3`, true), Options{Recorder: rec, Stdout: &out, Stderr: &errb, Signals: sigs})
	sigs <- syscall.SIGWINCH
	if got := waitStatus(t, ch, 5*time.Second); got != 3 {
		t.Fatalf("status %d, want 3", got)
	}
}

// The Review Focus case: a background grandchild keeps stdout open after the
// child exits, so Run must stop waiting for output after the drain.
func TestRunDoesNotWaitForGrandchildOutput(t *testing.T) {
	rec, _ := newRec(t)
	var out, errb lockedBuf
	c := start(t, `sleep 30 & echo hi; exit 4`, false)
	began := time.Now()
	ch := runAsync(c, Options{Recorder: rec, Stdout: &out, Stderr: &errb})
	if got := waitStatus(t, ch, 1500*time.Millisecond); got != 4 {
		t.Fatalf("status %d, want 4", got)
	}
	if took := time.Since(began); took > 1500*time.Millisecond {
		t.Fatalf("took %v", took)
	}
	if !strings.Contains(out.String(), "hi") {
		t.Fatalf("stdout %q lacks hi", out.String())
	}
}

type failWriter struct{}

func (failWriter) Write(p []byte) (int, error) { return 0, errors.New("write failed") }

func TestStdoutWriteFailureKeepsRecording(t *testing.T) {
	rec, s := newRec(t)
	var errb lockedBuf
	got := Run(start(t, `echo one; echo two; echo three; exit 2`, false), Options{Recorder: rec, Stdout: failWriter{}, Stderr: &errb})
	if got != 2 {
		t.Fatalf("status %d, want 2", got)
	}
	if recs := s.all(); !slices.Equal(recs, []string{"one", "two", "three"}) {
		t.Fatalf("sink %q", recs)
	}
}

// exec runs script with sh and returns its stdout, the reference bytes for a
// pass-through test.
func exec(script string) (string, error) {
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return "", err
	}
	defer devnull.Close()
	c, err := runner.StartChild([]string{"sh", "-c", script}, runner.Options{Stdin: devnull})
	if err != nil {
		return "", err
	}
	defer c.Close()
	var b bytes.Buffer
	if _, err := b.ReadFrom(c.Stdout()); err != nil {
		return "", err
	}
	<-c.Exited()
	return b.String(), nil
}

func TestStripANSI(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain", "plain"},
		{"a\x1b[31mred\x1b[0m", "ared"},
		{"\xff\xfe \x1b[1;2Hraw", "\xff\xfe raw"},
		{"\x1b]0;title\x07after", "after"},
		{"\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\", "link"},
		{"\x1b(Bkept\x1b=", "kept"},
		{"cut\x1b[3", "cut"},
		{"cut\x1b", "cut"},
		{"tab\there", "tab\there"},
	} {
		if got := string(stripANSI([]byte(tc.in))); got != tc.want {
			t.Errorf("stripANSI(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Ctrl-C starts the same grace as SIGTERM: a command still running after it
// is killed, and on a terminal a countdown says so from 500ms on.
func TestSIGINTCountdownThenKill(t *testing.T) {
	rec, _ := newRec(t)
	var out, errb, cd lockedBuf
	sigs := make(chan os.Signal, 1)
	ch := runAsync(start(t, `echo ready; while :; do sleep 0.05; done`, false),
		Options{Recorder: rec, Stdout: &out, Stderr: &errb, Signals: sigs, Grace: 2500 * time.Millisecond, Countdown: &cd, Name: "npm"})
	waitFor(t, &out, "ready")
	sigs <- syscall.SIGINT
	if got := waitStatus(t, ch, 5*time.Second); got != 137 {
		t.Fatalf("status %d, want 137", got)
	}
	for _, want := range []string{"loghorn: waiting for npm to exit; killing it in 2s", "killing it in 1s", "loghorn: killed npm"} {
		if !strings.Contains(cd.String(), want) {
			t.Fatalf("countdown %q lacks %q", cd.String(), want)
		}
	}
}

func TestSIGINTQuickExitShowsNoCountdown(t *testing.T) {
	rec, _ := newRec(t)
	var out, errb, cd lockedBuf
	sigs := make(chan os.Signal, 1)
	ch := runAsync(start(t, "sleep 0.2; exit 0", false),
		Options{Recorder: rec, Stdout: &out, Stderr: &errb, Signals: sigs, Grace: 5 * time.Second, Countdown: &cd, Name: "npm"})
	sigs <- syscall.SIGINT
	if got := waitStatus(t, ch, 5*time.Second); got != 0 {
		t.Fatalf("status %d, want 0", got)
	}
	if cd.String() != "" {
		t.Fatalf("countdown shown for a quick exit: %q", cd.String())
	}
}

func TestSecondSIGINTKillsNow(t *testing.T) {
	rec, _ := newRec(t)
	var out, errb, cd lockedBuf
	sigs := make(chan os.Signal, 2)
	ch := runAsync(start(t, `echo ready; while :; do sleep 0.05; done`, false),
		Options{Recorder: rec, Stdout: &out, Stderr: &errb, Signals: sigs, Grace: 10 * time.Second, Countdown: &cd, Name: "npm"})
	waitFor(t, &out, "ready")
	sigs <- syscall.SIGINT
	sigs <- syscall.SIGINT
	if got := waitStatus(t, ch, 2*time.Second); got != 137 {
		t.Fatalf("status %d, want 137", got)
	}
}
