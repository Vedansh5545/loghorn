package logfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSpanPruneSparesLogFiles(t *testing.T) {
	dir := t.TempDir()
	w, _ := openAt(t, dir, at(2026, 9, 15, 12, 0))
	putFile(t, filepath.Join(dir, "loghorn-2026-09-09.log"), "old", at(2026, 9, 9, 12, 0))
	putFile(t, filepath.Join(dir, "loghorn-2026-09-10.traces.jsonl"), "old", at(2026, 9, 10, 12, 0))
	putFile(t, filepath.Join(dir, "loghorn-2026-09-14.traces.jsonl"), "new", at(2026, 9, 14, 12, 0))

	if _, err := w.Companion(SpansBase, SpansSuffix); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(dir, "loghorn-2026-09-10.traces.jsonl")) {
		t.Errorf("old span archive should be pruned")
	}
	for _, n := range []string{"loghorn-2026-09-14.traces.jsonl", "loghorn-2026-09-09.log", "loghorn.log"} {
		if !exists(filepath.Join(dir, n)) {
			t.Errorf("%s should still exist", n)
		}
	}
}

func TestCompanionCreatesFileOnFirstWrite(t *testing.T) {
	dir := t.TempDir()
	w, _ := openAt(t, dir, at(2026, 9, 15, 12, 0))
	c, err := w.Companion(SpansBase, SpansSuffix)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "loghorn.traces.jsonl")
	if exists(p) {
		t.Fatalf("file must not exist before the first Write")
	}
	write(t, c, "a")
	if got := readFile(t, p); got != "a\n" {
		t.Fatalf("got %q", got)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	if got := readFile(t, filepath.Join(dir, "loghorn.log")); got != "" {
		t.Errorf("loghorn.log touched: %q", got)
	}
}

func TestCompanionArchivesLeftoverAtCreate(t *testing.T) {
	dir := t.TempDir()
	putFile(t, filepath.Join(dir, "loghorn.traces.jsonl"), "old\n", at(2026, 9, 14, 22, 0))
	w, _ := openAt(t, dir, at(2026, 9, 15, 10, 0))
	if _, err := w.Companion(SpansBase, SpansSuffix); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "loghorn-2026-09-14.traces.jsonl")); got != "old\n" {
		t.Errorf("archive = %q", got)
	}
	if exists(filepath.Join(dir, "loghorn.traces.jsonl")) {
		t.Errorf("loghorn.traces.jsonl should be absent")
	}
}

func TestCompanionFirstWriteAfterMidnight(t *testing.T) {
	dir := t.TempDir()
	putFile(t, filepath.Join(dir, "loghorn.traces.jsonl"), "old\n", at(2026, 9, 15, 9, 0))
	w, c := openAt(t, dir, at(2026, 9, 15, 10, 0))
	sw, err := w.Companion(SpansBase, SpansSuffix)
	if err != nil {
		t.Fatal(err)
	}
	c.t = at(2026, 9, 16, 1, 0)
	write(t, sw, "b")
	if got := readFile(t, filepath.Join(dir, "loghorn-2026-09-15.traces.jsonl")); got != "old\n" {
		t.Errorf("archive = %q", got)
	}
	if got := readFile(t, filepath.Join(dir, "loghorn.traces.jsonl")); got != "b\n" {
		t.Errorf("current = %q", got)
	}
}

func TestCompanionRollsOver(t *testing.T) {
	dir := t.TempDir()
	w, c := openAt(t, dir, at(2026, 9, 15, 10, 0))
	sw, err := w.Companion(SpansBase, SpansSuffix)
	if err != nil {
		t.Fatal(err)
	}
	write(t, w, "log")
	write(t, sw, "a")
	c.t = at(2026, 9, 16, 1, 0)
	write(t, sw, "b")
	if got := readFile(t, filepath.Join(dir, "loghorn-2026-09-15.traces.jsonl")); got != "a\n" {
		t.Errorf("span archive = %q", got)
	}
	if got := readFile(t, filepath.Join(dir, "loghorn.traces.jsonl")); got != "b\n" {
		t.Errorf("span current = %q", got)
	}
	if got := readFile(t, filepath.Join(dir, "loghorn.log")); got != "log\n" {
		t.Errorf("loghorn.log = %q", got)
	}
	if exists(filepath.Join(dir, "loghorn-2026-09-15.log")) {
		t.Errorf("the log writer must not have rolled over")
	}
}

func TestCompanionWriteAfterCloseCreatesNothing(t *testing.T) {
	for _, closeCompanion := range []bool{true, false} {
		dir := t.TempDir()
		w, _ := openAt(t, dir, at(2026, 9, 15, 10, 0))
		sw, err := w.Companion(SpansBase, SpansSuffix)
		if err != nil {
			t.Fatal(err)
		}
		if closeCompanion {
			sw.Close()
		} else {
			w.Close()
		}
		if err := sw.Write([]byte("late")); err != nil {
			t.Errorf("Write after close: %v", err)
		}
		if exists(filepath.Join(dir, "loghorn.traces.jsonl")) {
			t.Errorf("closeCompanion=%v: file created after close", closeCompanion)
		}
	}
}

func TestCloseClosesCompanionBeforeLock(t *testing.T) {
	dir := t.TempDir()
	w, c := openAt(t, dir, at(2026, 9, 15, 10, 0))
	sw, err := w.Companion(SpansBase, SpansSuffix)
	if err != nil {
		t.Fatal(err)
	}
	write(t, sw, "a")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	w2, err := Open(dir, c.Now, la)
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	defer w2.Close()
	if err := sw.Write([]byte("b")); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "loghorn.traces.jsonl")); got != "a\n" {
		t.Errorf("appended after Close: %q", got)
	}
}

func TestCompanionErrors(t *testing.T) {
	dir := t.TempDir()
	w, _ := openAt(t, dir, at(2026, 9, 15, 10, 0))
	if sw, err := w.Companion(LogBase, LogSuffix); err == nil || sw != nil {
		t.Errorf("own base: %v %v", sw, err)
	}
	w.Close()
	if sw, err := w.Companion(SpansBase, SpansSuffix); err == nil || sw != nil {
		t.Errorf("after Close: %v %v", sw, err)
	}
}

func TestCompanionWriteFailureLeavesLogWriter(t *testing.T) {
	dir := t.TempDir()
	w, _ := openAt(t, dir, at(2026, 9, 15, 10, 0))
	sw, err := w.Companion(SpansBase, SpansSuffix)
	if err != nil {
		t.Fatal(err)
	}
	write(t, sw, "a")
	sw.f.Close()
	if err := sw.Write([]byte("b")); err == nil {
		t.Fatalf("first failure should be returned")
	}
	if err := sw.Write([]byte("c")); err != nil {
		t.Fatalf("later writes must be silent: %v", err)
	}
	write(t, w, "log")
	if got := readFile(t, filepath.Join(dir, "loghorn.log")); got != "log\n" {
		t.Errorf("log writer: %q", got)
	}
}

func TestLogWriteFailureLeavesCompanion(t *testing.T) {
	dir := t.TempDir()
	w, _ := openAt(t, dir, at(2026, 9, 15, 10, 0))
	sw, err := w.Companion(SpansBase, SpansSuffix)
	if err != nil {
		t.Fatal(err)
	}
	w.f.Close()
	if err := w.Write([]byte("x")); err == nil {
		t.Fatalf("expected failure")
	}
	write(t, sw, "a")
	if got := readFile(t, filepath.Join(dir, "loghorn.traces.jsonl")); got != "a\n" {
		t.Errorf("companion: %q", got)
	}
	if _, err := w.Companion("other", ".x"); err != nil {
		t.Errorf("a stopped (not closed) writer may still hand out companions: %v", err)
	}
}
