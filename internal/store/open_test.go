package store

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/adapter"
	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/duckstore"
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

func total(s *Store) int {
	n := 0
	for _, c := range s.Streams() {
		n += c.Count
	}
	return n
}

// TestFirstOpenImportsTextOnce: the first writer imports the retained
// days' text; later opens find the records in the files.
func TestFirstOpenImportsTextOnce(t *testing.T) {
	dir := textDir(t)
	w := openStore(t, dir, ModeLive, true)
	if n := total(w.s); n != 6 {
		t.Fatalf("first open: %d records, want 6", n)
	}
	ids := seqs(w.s)
	w.s.Close()
	again := openStore(t, dir, ModeLive, true)
	defer again.s.Close()
	if n := total(again.s); n != 6 {
		t.Errorf("second open: %d records, want 6 (nothing imported twice)", n)
	}
	if got := seqs(again.s); !reflect.DeepEqual(got, ids) {
		t.Errorf("ids changed: %v, want %v", got, ids)
	}
	again.s.Add(logRecord(t, logLine(9, dayNow)), dayNow)
	if got := seqs(again.s); got[len(got)-1] != DayNumber(dayNow)<<32+5 {
		t.Errorf("next id %d continues today's sequence? want %d", got[len(got)-1], DayNumber(dayNow)<<32+5)
	}
}

func seqs(s *Store) []int {
	var out []int
	for _, r := range s.Records() {
		out = append(out, r.Seq)
	}
	return out
}

// TestInterruptedImportIsRedone: files without the imported mark hold a
// partial import, which is thrown away and done again.
func TestInterruptedImportIsRedone(t *testing.T) {
	dir := textDir(t)
	e, err := duckstore.OpenFiles(config.Input{}, nil, duckstore.FileOptions{Dir: dir, Write: true})
	if err != nil {
		t.Fatal(err)
	}
	e.Add(logRecord(t, logLine(1, dayNow)))
	e.Close()
	w := openStore(t, dir, ModeLive, true)
	defer w.s.Close()
	if n := total(w.s); n != 6 {
		t.Errorf("records = %d, want the 6 in the text", n)
	}
}

// TestRecordsPersistWithoutText: the files, not the text, hold the records
// once imported.
func TestRecordsPersistWithoutText(t *testing.T) {
	dir := t.TempDir()
	w := openStore(t, dir, ModeLive, true)
	w.s.Add(logRecord(t, logLine(1, dayNow)), dayNow)
	w.s.Add(logRecord(t, logLine(2, dayNow)), dayNow)
	w.s.Close()
	again := openStore(t, dir, ModeLive, true)
	defer again.s.Close()
	if n := total(again.s); n != 2 {
		t.Errorf("records = %d, want 2", n)
	}
}

// TestHistoricalReadsFilesOrText: a historical run reads the files when
// they exist, and the text when they don't.
func TestHistoricalReadsFilesOrText(t *testing.T) {
	dir := textDir(t)
	h := openStore(t, dir, ModeHistorical, false)
	if n := total(h.s); n != 6 || len(h.warns) != 0 {
		t.Errorf("without files: %d records, warnings %q", n, h.warns)
	}
	h.s.Close()
	w := openStore(t, dir, ModeLive, true)
	w.s.Add(logRecord(t, logLine(9, dayNow)), dayNow)
	w.s.Close()
	h = openStore(t, dir, ModeHistorical, false)
	defer h.s.Close()
	if n := total(h.s); n != 7 {
		t.Errorf("from files: %d records, want 7 (6 imported + 1 live)", n)
	}
}

// TestLiveReaderReadsText: a live run that doesn't own the files reads the
// retained days' text.
func TestLiveReaderReadsText(t *testing.T) {
	r := openStore(t, textDir(t), ModeLive, false)
	defer r.s.Close()
	if n := total(r.s); n != 6 {
		t.Errorf("records = %d, want 6", n)
	}
}

// TestDBDirSeparateFromTextDir: -db-dir puts the databases elsewhere.
func TestDBDirSeparateFromTextDir(t *testing.T) {
	dir, db := textDir(t), t.TempDir()
	s, err := Open(Options{Mode: ModeLive, Dir: dir, DBDir: db, Write: true, Now: func() time.Time { return dayNow }, Warn: func(string) {}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, f := range []string{"logs.duckdb", "traces.duckdb"} {
		if _, err := os.Stat(filepath.Join(db, f)); err != nil {
			t.Errorf("%s not in the db dir: %v", f, err)
		}
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("%s in the text dir", f)
		}
	}
	if n := total(s); n != 6 {
		t.Errorf("records = %d, want 6", n)
	}
}

// An imported day's untimed records take times from that day only: leading
// ones get their own day's first time, and a day with no time at all keeps
// its records untimed rather than leaving them pending.
func TestImportedDayTimesComeFromThatDay(t *testing.T) {
	dir := t.TempDir()
	untimed := `{"severity":"INFO","message":"u","labels":{"schema":"api"}}`
	d2 := dayNow.AddDate(0, 0, -2)
	y := dayNow.AddDate(0, 0, -1)
	writeText(t, dir, "loghorn-2026-11-21.log", d2, untimed)
	writeText(t, dir, "loghorn-2026-11-22.log", y, logLine(1, y))
	writeText(t, dir, "loghorn.log", dayNow.Add(-time.Hour), untimed, logLine(4, dayNow))
	o := openStore(t, dir, ModeLive, true)
	defer o.s.Close()
	want := map[int]time.Time{
		DayNumber(d2):     {},
		DayNumber(y):      y.Add(time.Minute).UTC(),
		DayNumber(dayNow): dayNow.Add(4 * time.Minute).UTC(),
	}
	for _, r := range o.s.Records() {
		if r.Fields["message"] != "u" {
			continue
		}
		d := r.Seq >> 32
		if !r.Time.Equal(want[d]) {
			t.Errorf("untimed record of day %d has time %v, want %v", d, r.Time, want[d])
		}
	}
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	if len(o.s.pending) != 0 {
		t.Errorf("%d records still pending after the import", len(o.s.pending))
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

// TestDeferredImportIsDoneByTheNextWebWriter: a writer without -web imports
// nothing; the next writer with -web starts the files from text (which has
// everything the deferred run wrote too).
func TestDeferredImportIsDoneByTheNextWebWriter(t *testing.T) {
	dir := textDir(t)
	d, err := Open(Options{Mode: ModeLive, Dir: dir, Write: true, DeferImport: true, Now: func() time.Time { return dayNow }, Warn: func(string) {}})
	if err != nil {
		t.Fatal(err)
	}
	if n := total(d); n != 0 {
		t.Errorf("deferred writer imported %d records", n)
	}
	d.Add(logRecord(t, logLine(9, dayNow)), dayNow)
	d.Close()
	w := openStore(t, dir, ModeLive, true)
	defer w.s.Close()
	if n := total(w.s); n != 6 {
		t.Errorf("records = %d, want the 6 in the text", n)
	}
}

// TestWriterArchivesClosedDays: opening the files archives the days before
// today to Parquet; the records read the same.
func TestWriterArchivesClosedDays(t *testing.T) {
	dir := textDir(t)
	w := openStore(t, dir, ModeLive, true)
	defer w.s.Close()
	if _, err := os.Stat(filepath.Join(dir, duckstore.ArchiveDir, "logs-2026-11-22.parquet")); err != nil {
		t.Errorf("yesterday's archive: %v", err)
	}
	if n := total(w.s); n != 6 {
		t.Errorf("records = %d, want 6", n)
	}
	if _, err := os.Stat(filepath.Join(dir, duckstore.ArchiveDir, "logs-2026-11-23.parquet")); err == nil {
		t.Error("today was archived")
	}
}

// TestHistoricalNeedsImportedFiles: files a deferred writer started hold
// only that run's records, so a historical run reads the text (review:
// Important 2).
func TestHistoricalNeedsImportedFiles(t *testing.T) {
	dir := textDir(t)
	d, err := Open(Options{Mode: ModeLive, Dir: dir, Write: true, DeferImport: true, Now: func() time.Time { return dayNow }, Warn: func(string) {}})
	if err != nil {
		t.Fatal(err)
	}
	d.Add(logRecord(t, logLine(9, dayNow)), dayNow)
	d.Close()
	h := openStore(t, dir, ModeHistorical, false)
	defer h.s.Close()
	if n := total(h.s); n != 6 {
		t.Errorf("records = %d, want the 6 in the text", n)
	}
}

// TestPartialImportIsNotMarked: a day whose text can't be read leaves the
// files unmarked, so the next writer imports again (review: Important 3).
func TestPartialImportIsNotMarked(t *testing.T) {
	dir := textDir(t)
	y := filepath.Join(dir, "loghorn-2026-11-22.log")
	if err := os.Chmod(y, 0); err != nil {
		t.Fatal(err)
	}
	w := openStore(t, dir, ModeLive, true)
	if v, _ := w.s.e.Meta("imported"); v == "1" {
		t.Error("a partial import was marked complete")
	}
	w.s.Close()
	os.Chmod(y, 0o600)
	again := openStore(t, dir, ModeLive, true)
	defer again.s.Close()
	if n := total(again.s); n != 6 {
		t.Errorf("after the retry: %d records, want 6", n)
	}
}
