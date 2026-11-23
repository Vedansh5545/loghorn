package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/sock"
	"github.com/jimbeveridge/loghorn/internal/store"
	"github.com/jimbeveridge/loghorn/internal/web"
)

const cliPino = "testdata/web/pino.log"

// allQuery names every record of the pino fixture (3 records in stream db).
const allQuery = `{"stream":"db","range":{"from":"all"}}`

// cliHandler is a web handler over the pino fixture, with instance identity.
func cliHandler(t *testing.T, pid, port int, cwd string) http.Handler {
	t.Helper()
	st := store.New(store.ModeHistorical, time.Time{})
	if err := store.Preload(st, []string{cliPino}, config.Input{}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 17, 18, 0, 0, 0, time.UTC)
	return web.New(web.Options{
		Store: st, ConfigBase: t.TempDir(), Now: func() time.Time { return now },
		PID: pid, Cwd: cwd, Port: port, Started: now,
	})
}

// bodyRecorder wraps a handler and keeps the body of the last /api/query.
type bodyRecorder struct {
	h    http.Handler
	mu   sync.Mutex
	last string
}

func (r *bodyRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/api/query" {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.last = string(b)
		r.mu.Unlock()
		req.Body = io.NopCloser(bytes.NewReader(b))
	}
	r.h.ServeHTTP(w, req)
}

func (r *bodyRecorder) body() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// cliTCP serves the fixture over TCP and returns the server and the request
// recorder.
func cliTCP(t *testing.T) (*httptest.Server, *bodyRecorder) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	rec := &bodyRecorder{h: cliHandler(t, 4001, 0, "/work/a")}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	return srv, rec
}

// qURL is the page URL of a query on base.
func qURL(base, q string) string { return base + "/?q=" + url.QueryEscape(q) }

type cliResult struct {
	code           int
	stdout, stderr string
}

// runCLI runs a query with stdin (nil = empty) against serve's socket at
// socket ("" = a path nothing listens on).
func runCLI(t *testing.T, o opts, stdin io.Reader, socket string) cliResult {
	t.Helper()
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	if socket == "" {
		socket = noSocket()
	}
	var out, errb bytes.Buffer
	code := runQuery(o, queryEnv{stdin: stdin, stdout: &out, stderr: &errb, socket: socket})
	return cliResult{code, out.String(), errb.String()}
}

// tsvRows is the number of data rows of TSV text (lines after the header).
func tsvRows(s string) int {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return len(lines) - 1
}

func TestIsQueryURL(t *testing.T) {
	for s, want := range map[string]bool{
		"http://127.0.0.1:60000/?q=%7B%7D": true,
		"http://localhost/?q=%7B%7D":       true,
		"http://localhost:8080/":           true,
		"http://127.0.0.1":                 true,
		"https://127.0.0.1/":               false,
		"http://example.com/":              false,
		"ls":                               false,
		"./http:":                          false,
		"":                                 false,
	} {
		if got := isQueryURL(s); got != want {
			t.Errorf("isQueryURL(%q) = %v; want %v", s, got, want)
		}
	}
}

func TestQueryPositionalTSV(t *testing.T) {
	srv, _ := cliTCP(t)
	r := runCLI(t, opts{url: qURL(srv.URL, allQuery)}, nil, "")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "\t") || tsvRows(r.stdout) != 3 {
		t.Fatalf("stdout = %q; want TSV with 3 rows", r.stdout)
	}
	// localhost works too.
	lh := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	r2 := runCLI(t, opts{url: qURL(lh, allQuery)}, nil, "")
	if r2.code != 0 || r2.stdout != r.stdout {
		t.Fatalf("localhost: exit %d %s; stdout differs: %q", r2.code, r2.stderr, r2.stdout)
	}
}

func TestQueryStdinIgnored(t *testing.T) {
	srv, _ := cliTCP(t)
	want := runCLI(t, opts{url: qURL(srv.URL, allQuery)}, nil, "")

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	if _, err := pw.Write([]byte("junk line\n")); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	got := runCLI(t, opts{url: qURL(srv.URL, allQuery)}, pr, "")
	if got.code != 0 || got.stdout != want.stdout {
		t.Fatalf("with piped stdin: exit %d %s; stdout %q; want %q", got.code, got.stderr, got.stdout, want.stdout)
	}
	left, _ := io.ReadAll(pr)
	if string(left) != "junk line\n" {
		t.Fatalf("stdin was read: %q left", left)
	}
}

func TestQueryURLAtFile(t *testing.T) {
	srv, _ := cliTCP(t)
	f := filepath.Join(t.TempDir(), "url.txt")
	if err := os.WriteFile(f, []byte("\n  "+qURL(srv.URL, allQuery)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runCLI(t, opts{url: "@" + f}, nil, "")
	if r.code != 0 || tsvRows(r.stdout) != 3 {
		t.Fatalf("exit %d %s; stdout %q", r.code, r.stderr, r.stdout)
	}
	r = runCLI(t, opts{url: "@" + filepath.Join(t.TempDir(), "missing")}, nil, "")
	if r.code != 2 {
		t.Fatalf("missing @file: exit %d; want 2", r.code)
	}
}

func TestQueryURLAtStdin(t *testing.T) {
	srv, _ := cliTCP(t)
	r := runCLI(t, opts{url: "@-"}, strings.NewReader("  "+qURL(srv.URL, allQuery)+"\n"), "")
	if r.code != 0 || tsvRows(r.stdout) != 3 {
		t.Fatalf("exit %d %s; stdout %q", r.code, r.stderr, r.stdout)
	}
	r = runCLI(t, opts{url: "@-"}, strings.NewReader(" \n"), "")
	if r.code != 2 {
		t.Fatalf("empty stdin: exit %d; want 2 (%s)", r.code, r.stderr)
	}
}

func TestReadURLArg(t *testing.T) {
	if v, err := readURLArg("http://x/", strings.NewReader("ignored")); err != nil || v != "http://x/" {
		t.Fatalf("plain = %q, %v", v, err)
	}
	if v, err := readURLArg("@-", strings.NewReader(" /?q=1 \n")); err != nil || v != "/?q=1" {
		t.Fatalf("@- = %q, %v", v, err)
	}
}

func TestQueryFormatFromExt(t *testing.T) {
	srv, _ := cliTCP(t)
	dir := t.TempDir()
	md := filepath.Join(dir, "out.md")
	r := runCLI(t, opts{url: qURL(srv.URL, allQuery), out: md}, nil, "")
	if r.code != 0 || r.stdout != "" {
		t.Fatalf("exit %d %s; stdout %q", r.code, r.stderr, r.stdout)
	}
	b, err := os.ReadFile(md)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), "|") || !strings.Contains(string(b), "| --- |") && !strings.Contains(string(b), "|---") {
		t.Fatalf("out.md is not Markdown: %q", b)
	}

	r = runCLI(t, opts{url: qURL(srv.URL, allQuery), out: md, format: "csv"}, nil, "")
	if r.code != 0 {
		t.Fatalf("exit %d %s", r.code, r.stderr)
	}
	b, _ = os.ReadFile(md)
	if strings.HasPrefix(string(b), "|") || !strings.Contains(strings.SplitN(string(b), "\n", 2)[0], ",") {
		t.Fatalf("-format csv wrote %q", b)
	}

	r = runCLI(t, opts{url: qURL(srv.URL, allQuery), format: "xml"}, nil, "")
	if r.code != 2 {
		t.Fatalf("-format xml: exit %d; want 2", r.code)
	}
}

func TestQueryOutputNotTouchedOnError(t *testing.T) {
	srv, _ := cliTCP(t)
	f := filepath.Join(t.TempDir(), "keep.csv")
	if err := os.WriteFile(f, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runCLI(t, opts{url: qURL(srv.URL, `{"stream":"db","source":"bogus"}`), out: f}, nil, "")
	if r.code != 1 {
		t.Fatalf("exit %d; want 1", r.code)
	}
	if b, _ := os.ReadFile(f); string(b) != "old" {
		t.Fatalf("output file overwritten on error: %q", b)
	}
}

func TestQueryToSQL(t *testing.T) {
	srv, rec := cliTCP(t)
	r := runCLI(t, opts{url: qURL(srv.URL, allQuery), toSQL: true}, nil, "")
	if r.code != 0 || !strings.Contains(r.stdout, `FROM logs."db"`) {
		t.Fatalf("exit %d %s; stdout %q", r.code, r.stderr, r.stdout)
	}
	if !strings.Contains(rec.body(), `"format":"sql"`) {
		t.Fatalf("request %s", rec.body())
	}
	sqlFile := filepath.Join(t.TempDir(), "q.sql")
	r = runCLI(t, opts{url: qURL(srv.URL, allQuery), out: sqlFile}, nil, "")
	if b, _ := os.ReadFile(sqlFile); r.code != 0 || !strings.Contains(string(b), `FROM logs."db"`) {
		t.Fatalf("-o q.sql: exit %d %s; file %q", r.code, r.stderr, b)
	}
	r = runCLI(t, opts{url: qURL(srv.URL, allQuery), toSQL: true, format: "csv"}, nil, "")
	if r.code != 2 {
		t.Fatalf("-to-sql -format csv: exit %d; want 2", r.code)
	}
}

func TestQueryLimit(t *testing.T) {
	srv, rec := cliTCP(t)
	r := runCLI(t, opts{url: qURL(srv.URL, allQuery), limit: 2, limitSet: true}, nil, "")
	if r.code != 0 || tsvRows(r.stdout) != 2 {
		t.Fatalf("samples -limit 2: exit %d %s; stdout %q", r.code, r.stderr, r.stdout)
	}
	if !strings.Contains(r.stderr, "loghorn:") {
		t.Fatalf("no truncation note on stderr: %q", r.stderr)
	}

	table := `{"stream":"db","range":{"from":"all"},"view":"table","groupBy":["message"]}`
	// Two messages ("query ok", "slow query"); -limit 1 keeps one group.
	r = runCLI(t, opts{url: qURL(srv.URL, table), limit: 1, limitSet: true}, nil, "")
	if r.code != 0 || tsvRows(r.stdout) != 1 {
		t.Fatalf("table -limit 1: exit %d %s; stdout %q", r.code, r.stderr, r.stdout)
	}
	if b := rec.body(); !strings.Contains(b, `"top":1`) {
		t.Fatalf("table request %s; want top 1", b)
	}

	heat := `{"stream":"db","range":{"from":"all"},"view":"heatmap"}`
	r = runCLI(t, opts{url: qURL(srv.URL, heat), limit: 1, limitSet: true}, nil, "")
	if r.code != 2 {
		t.Fatalf("heatmap -limit 1: exit %d; want 2", r.code)
	}
	r = runCLI(t, opts{url: qURL(srv.URL, allQuery), limit: 0, limitSet: true}, nil, "")
	if r.code != 2 {
		t.Fatalf("-limit 0: exit %d; want 2", r.code)
	}
}

func TestQueryForeignHost(t *testing.T) {
	q := "/?q=" + url.QueryEscape(allQuery)
	for _, u := range []string{"http://example.com" + q, "https://127.0.0.1:1" + q, "http://localhost.example.com" + q, "http://[::1]:1" + q} {
		r := runCLI(t, opts{url: u}, nil, "")
		if r.code != 2 {
			t.Errorf("%s: exit %d; want 2", u, r.code)
		}
	}
	// A positional URL that only looks loopback is refused, never run.
	r := runCLI(t, opts{url: "http://localhost.example.com" + q}, nil, "")
	if r.code != 2 {
		t.Fatalf("positional foreign host: exit %d; want 2", r.code)
	}
}

func TestQueryServerError(t *testing.T) {
	srv, _ := cliTCP(t)
	r := runCLI(t, opts{url: qURL(srv.URL, `{"stream":"db","source":"bogus"}`)}, nil, "")
	if r.code != 1 || !strings.HasPrefix(r.stderr, "loghorn: ") || !strings.Contains(r.stderr, "bogus") {
		t.Fatalf("exit %d; stderr %q", r.code, r.stderr)
	}
	r = runCLI(t, opts{url: srv.URL + "/?x=1"}, nil, "")
	if r.code != 2 {
		t.Fatalf("no q: exit %d; want 2", r.code)
	}
	r = runCLI(t, opts{url: srv.URL + "/?q=notjson"}, nil, "")
	if r.code != 2 {
		t.Fatalf("bad q: exit %d; want 2", r.code)
	}
}

func TestQueryWedgedInstance(t *testing.T) {
	old := sock.ResponseTimeout
	sock.ResponseTimeout = 100 * time.Millisecond
	t.Cleanup(func() { sock.ResponseTimeout = old })
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(func() { close(release); srv.Close() })
	start := time.Now()
	r := runCLI(t, opts{url: qURL(srv.URL, allQuery)}, nil, "")
	if r.code != 1 || !strings.Contains(r.stderr, "can't reach") {
		t.Fatalf("exit %d; stderr %q", r.code, r.stderr)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("gave up after %v", d)
	}
}

func TestQueryUnreachable(t *testing.T) {
	srv, _ := cliTCP(t)
	u := srv.URL
	srv.Close()
	r := runCLI(t, opts{url: qURL(u, allQuery)}, nil, "")
	if r.code != 1 || !strings.Contains(r.stderr, "loghorn status") {
		t.Fatalf("exit %d; stderr %q", r.code, r.stderr)
	}
}

func TestQueryCrumbsIgnored(t *testing.T) {
	srv, _ := cliTCP(t)
	r := runCLI(t, opts{url: qURL(srv.URL, allQuery) + "&trace=abc"}, nil, "")
	if r.code != 0 || tsvRows(r.stdout) != 3 || !strings.Contains(r.stderr, "trace") {
		t.Fatalf("exit %d; stderr %q", r.code, r.stderr)
	}
	r = runCLI(t, opts{url: qURL(srv.URL, allQuery) + "&agg=x"}, nil, "")
	if r.code != 0 || !strings.Contains(r.stderr, "aggregate") {
		t.Fatalf("exit %d; stderr %q", r.code, r.stderr)
	}
}

// sockPath is a short socket path in a fresh /tmp dir.
func sockPath(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "lh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return filepath.Join(d, "s.sock")
}

func TestQueryViaSocket(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := sockPath(t)
	hostless := "/?q=" + url.QueryEscape(allQuery)

	r := runCLI(t, opts{url: hostless}, nil, path)
	if r.code != 1 || !strings.Contains(r.stderr, "loghorn serve isn't running (no answer on "+path+")") {
		t.Fatalf("none: exit %d; stderr %q", r.code, r.stderr)
	}

	ln, err := sock.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: cliHandler(t, 4001, 5001, "")}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
	r = runCLI(t, opts{url: hostless}, nil, path)
	if r.code != 0 || tsvRows(r.stdout) != 3 {
		t.Fatalf("serve up: exit %d %s; stdout %q", r.code, r.stderr, r.stdout)
	}
}
