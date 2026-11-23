package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // America/Los_Angeles without relying on the host's zoneinfo

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/logfile"
	"github.com/jimbeveridge/loghorn/internal/store"
)

// logDir is where the log file lives and -historical reads from: .loghorn
// under the directory loghorn was started in, one per project.
func TestLogDir(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	got, err := logDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cwd, ".loghorn"); got != want {
		t.Fatalf("logDir() = %q, want %q", got, want)
	}
}

func TestRecordSinkWithNoWriterIsANoop(t *testing.T) {
	called := false
	sink := recordSink(nil, func(error) { called = true })
	sink([]byte("x"))
	if called {
		t.Fatalf("onFail must not be called when there is no log file")
	}
}

// A real Writer whose rollover cannot archive (a directory squats on the
// archive's name) fails on the first write after the fake clock crosses
// midnight; onFail must run exactly once even though the sink is called again.
func TestRecordSinkCallsOnFailOnce(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	clock := &fakeClock{t: time.Date(2026, 9, 14, 22, 0, 0, 0, loc)}
	w, err := logfile.Open(dir, clock.Now, loc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })

	if err := os.Mkdir(filepath.Join(dir, "loghorn-2026-09-14.log"), 0o755); err != nil {
		t.Fatal(err)
	}
	clock.t = time.Date(2026, 9, 15, 1, 0, 0, 0, loc)

	var failures int
	sink := recordSink(w, func(error) { failures++ })
	sink([]byte("a"))
	sink([]byte("b"))
	if failures != 1 {
		t.Fatalf("onFail called %d times, want 1", failures)
	}
}

func TestSpanSinkWithNoWriterIsNil(t *testing.T) {
	if openSpanLog(nil) != nil {
		t.Fatal("openSpanLog(nil) != nil")
	}
	if spanSink(nil, nil, func(error) {}) != nil {
		t.Fatal("spanSink(nil) must be nil so the receiver discards traces")
	}
}

// The span writer is a companion of the log: each line lands in
// loghorn.traces.jsonl, never in loghorn.log, and a span-file failure reports
// once (a directory squats on the archive name the midnight rollover needs).
func TestSpanSinkWritesCompanionAndFailsOnce(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	clock := &fakeClock{t: time.Date(2026, 9, 14, 22, 0, 0, 0, loc)}
	logs, err := logfile.Open(dir, clock.Now, loc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logs.Close() })
	spans := openSpanLog(logs)
	if spans == nil {
		t.Fatal("openSpanLog returned nil")
	}
	var failures int
	sink := spanSink(spans, nil, func(error) { failures++ })
	sink([]byte(`{"name":"a"}`))
	b, err := os.ReadFile(filepath.Join(dir, logfile.SpansBase+logfile.SpansSuffix))
	if err != nil || string(b) != "{\"name\":\"a\"}\n" {
		t.Fatalf("span file = %q, %v", b, err)
	}
	if lb, _ := os.ReadFile(filepath.Join(dir, "loghorn.log")); strings.Contains(string(lb), "name") {
		t.Fatalf("loghorn.log holds the span: %q", lb)
	}

	if err := os.Mkdir(filepath.Join(dir, "loghorn-2026-09-14"+logfile.SpansSuffix), 0o755); err != nil {
		t.Fatal(err)
	}
	clock.t = time.Date(2026, 9, 15, 1, 0, 0, 0, loc)
	sink([]byte(`{"name":"b"}`))
	sink([]byte(`{"name":"c"}`))
	if failures != 1 {
		t.Fatalf("onFail called %d times, want 1", failures)
	}
}

func spanFixture(t *testing.T) [][]byte {
	t.Helper()
	b, err := os.ReadFile("testdata/web/spans.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Split(bytes.TrimSpace(b), []byte("\n"))
}

func TestSpanSinkStoreOnly(t *testing.T) {
	st := store.New(store.ModeLive, time.Now())
	sink := spanSink(nil, st, func(error) { t.Error("onFail") })
	if sink == nil {
		t.Fatal("store-only spanSink is nil")
	}
	sink(spanFixture(t)[3])
	sink([]byte(`{"msg":"not a span"}`))
	want := []store.StreamCount{{Name: "db", Source: "otel", Count: 1}}
	if got := st.Streams(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Streams = %v, want %v", got, want)
	}
}

func TestSpanSinkAddsWhenFileFails(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	clock := &fakeClock{t: time.Date(2026, 9, 14, 22, 0, 0, 0, loc)}
	logs, err := logfile.Open(dir, clock.Now, loc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logs.Close() })
	spans := openSpanLog(logs)
	st := store.New(store.ModeLive, time.Now())
	var failures int
	sink := spanSink(spans, st, func(error) { failures++ })
	lines := spanFixture(t)
	sink(lines[0])
	if err := os.Mkdir(filepath.Join(dir, "loghorn-2026-09-14"+logfile.SpansSuffix), 0o755); err != nil {
		t.Fatal(err)
	}
	clock.t = time.Date(2026, 9, 15, 1, 0, 0, 0, loc)
	sink(lines[1])
	sink(lines[2])
	if failures != 1 {
		t.Fatalf("onFail called %d times, want 1", failures)
	}
	n := 0
	for _, c := range st.Streams() {
		n += c.Count
	}
	if n != 3 {
		t.Fatalf("store holds %d spans (%v), want 3", n, st.Streams())
	}
}

func TestLiveReaderStoreReadsStoredLogsAndSpans(t *testing.T) {
	dir := t.TempDir()
	pino, err := os.ReadFile("testdata/web/pino.log")
	if err != nil {
		t.Fatal(err)
	}
	lines := spanFixture(t)
	join := func(ls [][]byte) []byte { return append(bytes.Join(ls, []byte("\n")), '\n') }
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	files := map[string][]byte{
		"loghorn-" + yesterday + ".log":          pino,
		"loghorn-" + yesterday + ".traces.jsonl": join(lines[:4]),
		"loghorn.traces.jsonl":                   join(lines[4:]),
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(store.Options{Mode: store.ModeLive, RunStart: time.Now(), Dir: dir, Input: config.Input{}})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got := map[store.StreamCount]bool{}
	for _, c := range st.Streams() {
		got[c] = true
	}
	for _, w := range []store.StreamCount{{Name: "db", Source: "log", Count: 3}, {Name: "db", Source: "otel", Count: 2}, {Name: "db open", Source: "otel", Count: 1}, {Name: "job.run", Source: "otel", Count: 1}} {
		if !got[w] {
			t.Errorf("missing %v in %v", w, st.Streams())
		}
	}
	recs := st.Records()
	if id := recs[len(recs)-1].Fields["span_id"]; id != "a000000000000009" {
		t.Errorf("last record span_id = %v, want a000000000000009", id)
	}
}

// recordSink must compact a record on its way to the log file: the file holds
// one record per line, and -historical replay depends on it. Asserted here
// because nothing else did — removing the CompactRecord call from recordSink
// left the whole suite passing.
func TestRecordSinkCompactsMultiLineRecords(t *testing.T) {
	dir := t.TempDir()
	w, err := logfile.Open(dir, time.Now, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	var failures int
	sink := recordSink(w, func(error) { failures++ })
	sink([]byte("{\n  \"severity\": 500,\n  \"insert_id\": \"a1\"\n}"))
	// A YAML document is the one record allowed to stay multi-line, because
	// replay re-detects its framing and re-groups it.
	sink([]byte("---\nseverity: 500\nmessage: keeps its lines"))
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if failures != 0 {
		t.Fatalf("onFail ran %d times, want 0", failures)
	}

	b, err := os.ReadFile(filepath.Join(dir, "loghorn.log"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.Contains(got, `{"severity":500,"insert_id":"a1"}`+"\n") {
		t.Errorf("the multi-line JSON record was not compacted onto one line:\n%q", got)
	}
	if !strings.Contains(got, "---\nseverity: 500\nmessage: keeps its lines\n") {
		t.Errorf("the YAML document should keep its newlines:\n%q", got)
	}
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }

// main resolves its config with config.Load(cwd): the working directory
// anchors it, the same as .loghorn/ and the source-tree refusal, so all three
// agree on what "this project" means. This pins that call's contract —
// coverage of the walk itself belongs to package config.
func TestConfigForUsesWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, config.RelPath)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("[input]\nformat = \"json\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, path, err := config.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if path != p || cfg.Input.Format != config.FormatJSON {
		t.Fatalf("Load(%q) = %q, %q; want %q, json", dir, path, cfg.Input.Format, p)
	}
}

func TestWebStoreMode(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name                          string
		historical, ghMode, fileInput bool
		want                          store.Mode
		wantStart                     time.Time
	}{
		{"live", false, false, false, store.ModeLive, now},
		{"historical", true, false, false, store.ModeHistorical, time.Time{}},
		{"gh", false, true, false, store.ModeGHJob, time.Time{}},
		{"stdin from file", false, false, true, store.ModeHistorical, time.Time{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mode, start := webStoreMode(c.historical, c.ghMode, c.fileInput, now)
			if mode != c.want || !start.Equal(c.wantStart) {
				t.Fatalf("webStoreMode = %v, %v; want %v, %v", mode, start, c.want, c.wantStart)
			}
		})
	}
}
