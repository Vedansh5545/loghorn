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

func openStore(t *testing.T, dir string, mode Mode) *opened {
	t.Helper()
	o := &opened{}
	s, err := Open(Options{Mode: mode, Dir: dir, Input: config.Input{},
		Now: func() time.Time { return dayNow }, Warn: func(m string) { o.warns = append(o.warns, m) }})
	if err != nil {
		t.Fatal(err)
	}
	o.s = s
	return o
}

func total(s *Store) int {
	n := 0
	streams, _ := s.Streams()
	for _, c := range streams {
		n += c.Count
	}
	return n
}

// TestFirstOpenImportsTextOnce: the first writer imports the retained
// days' text; later opens find the records in the files.
func TestFirstOpenImportsTextOnce(t *testing.T) {
	dir := textDir(t)
	w := openStore(t, dir, ModeLive)
	if n := total(w.s); n != 6 {
		t.Fatalf("first open: %d records, want 6", n)
	}
	ids := seqs(w.s)
	w.s.Close()
	again := openStore(t, dir, ModeLive)
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
	e, err := duckstore.OpenFiles(config.Input{}, nil, duckstore.FileOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	e.Add(logRecord(t, logLine(1, dayNow)))
	e.Close()
	w := openStore(t, dir, ModeLive)
	defer w.s.Close()
	if n := total(w.s); n != 6 {
		t.Errorf("records = %d, want the 6 in the text", n)
	}
}

// TestRecordsPersistWithoutText: the files, not the text, hold the records
// once imported.
func TestRecordsPersistWithoutText(t *testing.T) {
	dir := t.TempDir()
	w := openStore(t, dir, ModeLive)
	w.s.Add(logRecord(t, logLine(1, dayNow)), dayNow)
	w.s.Add(logRecord(t, logLine(2, dayNow)), dayNow)
	w.s.Close()
	again := openStore(t, dir, ModeLive)
	defer again.s.Close()
	if n := total(again.s); n != 2 {
		t.Errorf("records = %d, want 2", n)
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
	o := openStore(t, dir, ModeLive)
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

// TestWriterArchivesClosedDays: opening the files archives the days before
// today to Parquet; the records read the same.
func TestWriterArchivesClosedDays(t *testing.T) {
	dir := textDir(t)
	w := openStore(t, dir, ModeLive)
	defer w.s.Close()
	if _, err := os.Stat(filepath.Join(dir, duckstore.ArchiveDir, "records-2026-11-22.parquet")); err != nil {
		t.Errorf("yesterday's archive: %v", err)
	}
	if n := total(w.s); n != 6 {
		t.Errorf("records = %d, want 6", n)
	}
	if _, err := os.Stat(filepath.Join(dir, duckstore.ArchiveDir, "records-2026-11-23.parquet")); err == nil {
		t.Error("today was archived")
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
	w := openStore(t, dir, ModeLive)
	w.s.Close()
	if _, err := os.Stat(filepath.Join(dir, duckstore.ImportedFile)); err == nil {
		t.Error("a partial import was marked complete")
	}
	os.Chmod(y, 0o600)
	again := openStore(t, dir, ModeLive)
	defer again.s.Close()
	if n := total(again.s); n != 6 {
		t.Errorf("after the retry: %d records, want 6", n)
	}
}

// TestImportedMarkClearedWhileOpen: the mark is cleared when the files are
// opened and set again only by a clean Close. A crash leaves it cleared,
// and so does an open that fails, after which serve keeps that session's
// records in memory and text only: the next open imports them from the
// text (review §1.3).
func TestImportedMarkClearedWhileOpen(t *testing.T) {
	dir := textDir(t)
	mark := filepath.Join(dir, duckstore.ImportedFile)
	w := openStore(t, dir, ModeLive)
	if _, err := os.Stat(mark); err == nil {
		t.Error("files marked complete while open")
	}
	w.s.Close()
	if _, err := os.Stat(mark); err != nil {
		t.Fatalf("a clean close left no mark: %v", err)
	}
	db := filepath.Join(dir, duckstore.FileName)
	if err := os.Chmod(db, 0); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(Options{Mode: ModeLive, Dir: dir, Now: func() time.Time { return dayNow }, Warn: func(string) {}}); err == nil {
		s.Close()
		t.Fatal("opened an unreadable database file")
	}
	if _, err := os.Stat(mark); err == nil {
		t.Error("a failed open left the mark")
	}
	os.Chmod(db, 0o600)
	writeText(t, dir, "loghorn.log", dayNow.Add(-time.Minute), logLine(6, dayNow)) // written during the session in memory
	again := openStore(t, dir, ModeLive)
	defer again.s.Close()
	if n := total(again.s); n != 7 {
		t.Errorf("after the failed open: %d records, want the 7 in the text", n)
	}
}
