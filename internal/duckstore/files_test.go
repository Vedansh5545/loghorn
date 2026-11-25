package duckstore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/record"
)

func openFiles(t *testing.T, dir string, write bool, warn func(string)) *Engine {
	t.Helper()
	e, err := OpenFiles(config.Input{}, warn, FileOptions{Dir: dir, Write: write, KeepDays: 3})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func addSync(t *testing.T, e *Engine, rs ...record.Record) {
	t.Helper()
	for _, r := range rs {
		e.Add(r)
	}
	if !e.Sync(5 * time.Second) {
		t.Fatal("sync timed out")
	}
}

func withSeq(r record.Record, seq int) record.Record { r.Seq = seq; return r }

func TestFilesPersistAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	day := 20000
	e := openFiles(t, dir, true, nil)
	addSync(t, e,
		withSeq(logRec(t, "s", "api", `,"n":1`), day<<32+1),
		withSeq(spanRec(t, `{"trace_id":"0af7651916cd43dd8448eb211c80319c","span_id":"a000000000000001","name":"chat","kind":"client","start":"2026-11-17T17:01:01.000Z","end":"2026-11-17T17:01:02.500Z","duration_ms":1500,"service.name":"svc"}`), day<<32+2),
	)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "loghorn.duckdb")); err != nil {
		t.Fatal(err)
	}
	e = openFiles(t, dir, true, nil)
	defer e.Close()
	// One file, one records table for logs and spans (spec: Layout D).
	var tables []string
	rows, err := e.DB().Query("SELECT table_name FROM duckdb_tables() WHERE database_name = 'loghorn_db' ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	if strings.Join(tables, ",") != "_fields,_meta,records" {
		t.Errorf("tables %v, want _fields, _meta, records", tables)
	}
	var sources int
	if err := e.DB().QueryRow("SELECT count(DISTINCT _source) FROM loghorn_db.main.records").Scan(&sources); err != nil || sources != 2 {
		t.Errorf("records table holds %d sources (%v), want logs and spans", sources, err)
	}
	recs, err := e.Records(context.Background(), "true")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].Seq != day<<32+1 || recs[1].Seq != day<<32+2 || recs[1].Source != record.SourceOtel {
		t.Fatalf("records after reopen: %+v", recs)
	}
	if got := e.MaxSeq(day); got != 2 {
		t.Errorf("MaxSeq = %d, want 2", got)
	}
	tl, err := e.Tallies(record.SourceLog, "s", "api")
	if err != nil || tl["n"].Count != 1 {
		t.Errorf("tallies after reopen: %v, %v", tl, err)
	}
}

func TestReadOnlyOpenNeedsFiles(t *testing.T) {
	dir := t.TempDir()
	if e, err := OpenFiles(config.Input{}, nil, FileOptions{Dir: dir}); err == nil {
		e.Close()
		t.Fatal("read-only open of an empty directory succeeded")
	}
	// DuckDB's lock keeps another process out while a writer has the files
	// open; within one process it doesn't, so that isn't tested here.
	w := openFiles(t, dir, true, nil)
	addSync(t, w, withSeq(logRec(t, "s", "api", ``), 1))
	w.Close()
	r := openFiles(t, dir, false, nil)
	defer r.Close()
	recs, err := r.Records(context.Background(), "true")
	if err != nil || len(recs) != 1 {
		t.Fatalf("read-only records: %v, %v", recs, err)
	}
	var n int
	if err := r.DB().QueryRow(`SELECT count(*) FROM duckdb_databases() WHERE database_name LIKE '%_db'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("a reader keeps %d files attached (%v)", n, err)
	}
	if tl, _ := r.Tallies(record.SourceLog, "s", "api"); tl["labels.schema"].Count != 1 {
		t.Errorf("reader tallies %v", tl)
	}
}

func TestWriteFailureFallsBackToMemory(t *testing.T) {
	var warnings []string
	e := openFiles(t, t.TempDir(), true, func(m string) { warnings = append(warnings, m) })
	defer e.Close()
	addSync(t, e, withSeq(logRec(t, "s", "api", ``), 1))
	e.wmu.Lock()
	e.failWrites = true
	e.wmu.Unlock()
	addSync(t, e, withSeq(logRec(t, "s", "api", ``), 2))
	addSync(t, e, withSeq(logRec(t, "s", "api", ``), 3))
	recs, err := e.Records(context.Background(), "true")
	if err != nil || len(recs) != 3 {
		t.Fatalf("records: %d, %v", len(recs), err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "DuckDB write failed") {
		t.Errorf("warnings %q", warnings)
	}
	tl, _ := e.Tallies(record.SourceLog, "s", "api")
	if tl["labels.schema"].Count != 3 {
		t.Errorf("tallies count %d, want 3", tl["labels.schema"].Count)
	}
}

func TestFilesAreOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	e := openFiles(t, dir, true, nil)
	addSync(t, e, withSeq(logRec(t, "s", "api", ``), 1))
	defer e.Close()
	matches, _ := filepath.Glob(filepath.Join(dir, "*.duckdb*"))
	if len(matches) < 2 {
		t.Fatalf("files %v", matches)
	}
	for _, m := range matches {
		fi, err := os.Stat(m)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v", filepath.Base(m), fi.Mode().Perm())
		}
	}
}

func TestMetaPersists(t *testing.T) {
	dir := t.TempDir()
	e := openFiles(t, dir, true, nil)
	if v, err := e.Meta("imported"); err != nil || v != "" {
		t.Errorf("meta in new files: %q, %v", v, err)
	}
	if err := e.SetMeta("imported", "1"); err != nil {
		t.Fatal(err)
	}
	e.Close()
	e = openFiles(t, dir, true, nil)
	defer e.Close()
	if v, err := e.Meta("imported"); err != nil || v != "1" {
		t.Errorf("meta after reopen = %q, %v", v, err)
	}
}

// TestReaderReadsMeta: a reader sees the writer's marks (review: Important 2).
func TestReaderReadsMeta(t *testing.T) {
	dir := t.TempDir()
	w := openFiles(t, dir, true, nil)
	w.Close()
	r := openFiles(t, dir, false, nil)
	if v, err := r.Meta("imported"); err != nil || v != "" {
		t.Errorf("unmarked: %q, %v", v, err)
	}
	r.Close()
	w = openFiles(t, dir, true, nil)
	w.SetMeta("imported", "1")
	w.Close()
	r = openFiles(t, dir, false, nil)
	defer r.Close()
	if v, err := r.Meta("imported"); err != nil || v != "1" {
		t.Errorf("marked: %q, %v", v, err)
	}
}

// TestSkippedRecordsUnmarkImport: records skipped for a full queue are
// counted, and the files lose their imported mark so the next import
// brings them back from text (review: Important 5).
func TestSkippedRecordsUnmarkImport(t *testing.T) {
	dir := t.TempDir()
	var warns []string
	e := openFiles(t, dir, true, func(m string) { warns = append(warns, m) })
	e.SetMeta("imported", "1")
	e.SetMaxQueue(1)
	e.wmu.Lock() // the writer stalls, as during a long archive
	for i := 1; i <= 3; i++ {
		e.Add(withSeq(logRec(t, "s", "api", ``), i))
	}
	e.wmu.Unlock()
	e.Close()
	if !strings.Contains(strings.Join(warns, "\n"), "skipped") {
		t.Errorf("warnings %q", warns)
	}
	r := openFiles(t, dir, false, nil)
	defer r.Close()
	if v, _ := r.Meta("imported"); v == "1" {
		t.Error("files with skipped records still marked imported")
	}
}
