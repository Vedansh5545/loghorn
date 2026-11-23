package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/adapter"
	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/record"
)

var dayNow = time.Date(2026, 11, 23, 12, 0, 0, 0, time.Local)

func logLine(i int, day time.Time) string {
	return fmt.Sprintf(`{"timestamp":%q,"severity":"INFO","message":"m%d","labels":{"schema":"api"},"n":%d}`, day.Add(time.Duration(i)*time.Minute).UTC().Format(time.RFC3339), i, i)
}

func writeText(t *testing.T, dir, name string, mtime time.Time, lines ...string) {
	t.Helper()
	p := filepath.Join(dir, name)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(strings.Join(lines, "\n") + "\n")
	f.Close()
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// textDir is a .loghorn with yesterday's archive and today's current logs
// and spans.
func textDir(t *testing.T) string {
	dir := t.TempDir()
	y := dayNow.AddDate(0, 0, -1)
	writeText(t, dir, "loghorn-2026-11-22.log", y, logLine(1, y), logLine(2, y))
	writeText(t, dir, "loghorn.log", dayNow.Add(-time.Hour), logLine(3, dayNow), logLine(4, dayNow), logLine(5, dayNow))
	writeText(t, dir, "loghorn.traces.jsonl", dayNow.Add(-time.Hour),
		`{"trace_id":"0000000000000000000000000000000a","span_id":"000000000000000b","name":"q","kind":"client","start":"2026-11-23T10:00:00Z","end":"2026-11-23T10:00:01Z","duration_ms":1,"attributes":{"db.system":"sqlite"},"service.name":"s"}`)
	return dir
}

type opened struct {
	s     *Store
	warns []string
}

func openStore(t *testing.T, dir string, mode Mode, write bool) *opened {
	t.Helper()
	o := &opened{}
	s, err := Open(Options{Mode: mode, Dir: dir, Write: write, Input: config.Input{},
		Now: func() time.Time { return dayNow }, Warn: func(m string) { o.warns = append(o.warns, m) }})
	if err != nil {
		t.Fatal(err)
	}
	o.s = s
	return o
}

func (o *opened) built() []string {
	var out []string
	for _, w := range o.warns {
		if strings.Contains(w, "building DuckDB") {
			out = append(out, w[strings.Index(w, "for ")+4:][:10])
		}
	}
	return out
}

func total(s *Store) int {
	n := 0
	for _, c := range s.Streams() {
		n += c.Count
	}
	return n
}

func TestOpenWriterBuildsDaysFromTextOnce(t *testing.T) {
	dir := textDir(t)
	w := openStore(t, dir, ModeLive, true)
	if got := w.built(); strings.Join(got, ",") != "2026-11-22,2026-11-23" {
		t.Fatalf("built %v, want yesterday and today", got)
	}
	if n := total(w.s); n != 6 {
		t.Fatalf("records = %d, want 6", n)
	}
	w.s.Add(logRecord(t, logLine(9, dayNow)), dayNow)
	w.s.Close()
	for _, p := range []string{"logs-2026-11-22.duckdb", "traces-2026-11-23.duckdb"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}

	again := openStore(t, dir, ModeLive, true)
	defer again.s.Close()
	if got := again.built(); len(got) != 0 {
		t.Errorf("rebuilt %v on a clean reopen", got)
	}
	if n := total(again.s); n != 7 {
		t.Errorf("records after reopen = %d, want 7", n)
	}
	again.s.Add(logRecord(t, logLine(10, dayNow)), dayNow)
	recs := again.s.Records()
	last := recs[len(recs)-1]
	if want := DayNumber(dayNow)<<32 + 6; last.Seq != want {
		t.Errorf("next id = %d, want %d (today's sequence resumed)", last.Seq, want)
	}
}

func TestOpenWriterRebuildsDayWhoseTextChanged(t *testing.T) {
	dir := textDir(t)
	w := openStore(t, dir, ModeLive, true)
	w.s.Close()
	y := dayNow.AddDate(0, 0, -1)
	writeText(t, dir, "loghorn-2026-11-22.log", y, logLine(7, y))
	again := openStore(t, dir, ModeLive, true)
	defer again.s.Close()
	if got := again.built(); strings.Join(got, ",") != "2026-11-22" {
		t.Errorf("built %v, want only 2026-11-22", got)
	}
	if n := total(again.s); n != 7 {
		t.Errorf("records = %d, want 7", n)
	}
}

func TestOpenReaderUsesPairsAndTodaysText(t *testing.T) {
	dir := textDir(t)
	openStore(t, dir, ModeLive, true).s.Close()
	before, _ := os.ReadFile(filepath.Join(dir, "logs-2026-11-22.duckdb"))
	r := openStore(t, dir, ModeLive, false)
	if len(r.built()) != 0 {
		t.Errorf("a reader built %v", r.built())
	}
	r.s.Add(logRecord(t, logLine(8, dayNow)), dayNow)
	if n := total(r.s); n != 7 {
		t.Errorf("records = %d, want 6 stored + 1 live", n)
	}
	r.s.Close()
	after, _ := os.ReadFile(filepath.Join(dir, "logs-2026-11-22.duckdb"))
	if string(before) != string(after) {
		t.Error("a reader changed a day file")
	}
}

func TestOpenStoredReadsBrokenDayFromText(t *testing.T) {
	dir := textDir(t)
	openStore(t, dir, ModeLive, true).s.Close()
	if err := os.WriteFile(filepath.Join(dir, "logs-2026-11-22.duckdb"), []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := openStore(t, dir, ModeHistorical, false)
	defer h.s.Close()
	if n := total(h.s); n != 6 {
		t.Errorf("records = %d, want 6", n)
	}
}

func TestOpenWriterExpiresOldPairs(t *testing.T) {
	dir := textDir(t)
	for _, p := range []string{"logs-2026-11-10.duckdb", "traces-2026-11-10.duckdb"} {
		os.WriteFile(filepath.Join(dir, p), []byte("x"), 0o600)
	}
	w := openStore(t, dir, ModeLive, true)
	defer w.s.Close()
	if _, err := os.Stat(filepath.Join(dir, "logs-2026-11-10.duckdb")); !os.IsNotExist(err) {
		t.Errorf("old pair kept: %v", err)
	}
}

func logRecord(t *testing.T, line string) record.Record {
	t.Helper()
	r, ok := record.Normalize(adapter.New(config.Input{}).ParseLine([]byte(line)), config.Input{})
	if !ok {
		t.Fatalf("not a record: %s", line)
	}
	return r
}
