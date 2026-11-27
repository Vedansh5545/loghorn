package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/adapter"
	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/store"
)

func TestOpenSource(t *testing.T) {
	const run = "https://github.com/o/r/actions/runs/43954926722"
	const job = run + "/job/133607019104"
	for _, c := range []struct {
		pos, job, run string
		want          openSrc
		err           bool
	}{
		{pos: "log.txt", want: openSrc{"file", "log.txt"}},
		{pos: "-", want: openSrc{"stdin", ""}},
		{pos: job, want: openSrc{"gh-job", job}},
		{pos: run, want: openSrc{"gh-run", run}},
		{job: "5", want: openSrc{"gh-job", "5"}},
		{run: "6", want: openSrc{"gh-run", "6"}},
		{pos: "a.log", job: "5", err: true},
		{job: "5", run: "6", err: true},
		{err: true},
	} {
		got, err := openSource(c.pos, c.job, c.run)
		if (err != nil) != c.err || got != c.want {
			t.Errorf("openSource(%q, %q, %q) = %+v, %v", c.pos, c.job, c.run, got, err)
		}
	}
}

const (
	openErrLine  = `{"labels":{"schema":"app"},"severity":"ERROR","message":"e"}`
	openInfoLine = `{"labels":{"schema":"app"},"severity":"INFO","message":"i"}`
)

func openFixture(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "in.log")
	if err := os.WriteFile(p, []byte(openErrLine+"\n"+openInfoLine+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOpenFileFilter(t *testing.T) {
	e := newServeEnv(t)
	r := e.run(t, nil, nil, "open", "-filter", openFixture(t))
	if r.code != 0 || r.stdout != openErrLine+"\n" {
		t.Fatalf("code %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	if !empty(t, e.data) {
		t.Fatal("open wrote to the data folder")
	}
}

func TestOpenStdinFilterNotRecorded(t *testing.T) {
	e := newServeEnv(t)
	f, err := os.Open(openFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r := e.run(t, f, nil, "open", "-filter", "-")
	if r.code != 0 || r.stdout != openErrLine+"\n" || !empty(t, e.data) {
		t.Fatalf("code %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
}

func TestOpenFileServesViewWithoutSocket(t *testing.T) {
	e := newServeEnv(t)
	tmp, err := os.MkdirTemp("/tmp", "lht")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	cmd := e.cmd(t, context.Background(), "open", openFixture(t))
	cmd.Env = append(cmd.Env, "TMPDIR="+tmp)
	out := newWatchWriter("web view at http://127.0.0.1:")
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	select {
	case <-out.seen:
	case <-time.After(10 * time.Second):
		t.Fatalf("no URL; stderr %q", out.String())
	}
	m := regexp.MustCompile(`web view at (http://127\.0\.0\.1:\d+/)`).FindStringSubmatch(out.String())
	resp, err := http.Get(m[1] + "api/streams")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("streams = %d", resp.StatusCode)
	}
	if socks, _ := filepath.Glob(filepath.Join(tmp, "loghorn", "*.sock")); len(socks) != 0 {
		t.Fatalf("open made a socket: %v", socks)
	}
	if _, err := os.Lstat(e.socket); err == nil {
		t.Fatal("open listened on serve's socket")
	}
	cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("open exited with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("open did not stop on SIGTERM")
	}
	if !strings.Contains(out.String(), "web view at") {
		t.Fatal(out.String())
	}
}

// TestOpenViewUsesTheInputConfig: loghorn open's store rebuilds lines for
// display with the input config the recorder normalized them with, so a
// shown record matches its stored fields (review §5.3).
func TestOpenViewUsesTheInputConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	in := config.Input{Severity: config.SeverityNumeric}
	ws := startView(0, t.TempDir(), store.ModeHistorical, in)
	defer ws.srv.Close()
	line := []byte(`{"labels":{"schema":"web"},"severity":"ERROR","message":"m"}`)
	r, ok := record.Normalize(adapter.New(in).ParseLine(line), in)
	if !ok {
		t.Fatal("not a record")
	}
	ws.st.Add(r, time.Time{})
	recs := ws.st.Records()
	if len(recs) != 1 || recs[0].Fields["severity"] != "DEFAULT" {
		t.Fatalf("records %v; with severity = numeric the text ERROR is DEFAULT", recs)
	}
}
