package store

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jimbeveridge/loghorn/internal/adapter"
	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/duckstore"
	"github.com/jimbeveridge/loghorn/internal/ingest"
	"github.com/jimbeveridge/loghorn/internal/logfile"
	"github.com/jimbeveridge/loghorn/internal/record"
)

// KeepDays is how many days before today are kept, as for the text files.
const KeepDays = 3

// Options opens a store over a .loghorn directory.
type Options struct {
	Mode     Mode
	RunStart time.Time
	Input    config.Input
	Dir      string // the .loghorn directory, with the text logs
	// DBDir holds logs.duckdb, traces.duckdb and the Parquet archive; ""
	// means Dir.
	DBDir string
	// Write is true in the process holding loghorn.lock (with or without
	// -web): it owns the database files, writes and archives them.
	Write bool
	// DeferImport, with Write, is a run without -web: nothing reads the
	// files this run, so an import from text is left to the next writer
	// with -web and the command starts at once (D-027, adapting D-024).
	// Records still go to the files; that import starts them again from
	// text.
	DeferImport bool
	Now         func() time.Time // nil: time.Now
	Warn        func(string)     // nil: print to stderr
}

// Open opens a store over the retained days (today and KeepDays before).
//
//   - Write: the database files, created if missing. Files not yet filled
//     from text (new, cut short, or deferred) are started again from the
//     retained days' text, once; after that the files hold the records.
//     Closed days then move to Parquet archives, and expired ones go.
//   - Live without Write: the owner has the files, so the retained days are
//     read from their text into memory.
//   - Otherwise (stored days): the files' and archives' records, copied
//     into memory, or the text when there are no files, they can't be
//     read, or they don't hold every record yet (not marked imported).
func Open(o Options) (*Store, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Warn == nil {
		o.Warn = func(m string) { fmt.Fprintln(os.Stderr, m) }
	}
	if o.DBDir == "" {
		o.DBDir = o.Dir
	}
	today := DayNumber(o.Now())
	switch {
	case o.Write:
		return openWriter(o, today)
	case o.Mode == ModeLive:
		return openText(o, today)
	}
	e, err := duckstore.OpenFiles(o.Input, o.Warn, fileOptions(o, false))
	if err != nil {
		if _, serr := os.Stat(filepath.Join(o.DBDir, duckstore.FileNames[record.SourceLog])); serr == nil {
			o.Warn("loghorn: DuckDB: can't read the stored records (" + firstLine(err) + "); reading the text logs")
		}
		return openText(o, today)
	}
	if v, _ := e.Meta(duckstore.ImportedKey); v != "1" {
		// The text has everything the files have, and more.
		e.Close()
		return openText(o, today)
	}
	return newStore(e, o), nil
}

func fileOptions(o Options, write bool) duckstore.FileOptions {
	return duckstore.FileOptions{Dir: o.DBDir, Write: write, KeepDays: KeepDays}
}

func newStore(e *duckstore.Engine, o Options) *Store {
	s := wrap(e, o.Mode, o.RunStart)
	s.now = o.Now
	return s
}

// openWriter opens the files for writing, importing the text first if they
// haven't been filled from it yet.
func openWriter(o Options, today int) (*Store, error) {
	e, err := duckstore.OpenFiles(o.Input, o.Warn, fileOptions(o, true))
	if err != nil {
		return nil, err
	}
	v, err := e.Meta(duckstore.ImportedKey)
	if err != nil {
		// Unreadable marks must not look like files to start again.
		e.Close()
		return nil, err
	}
	if v != "1" && !o.DeferImport {
		if e.Count() > 0 || archived(o.DBDir) {
			// Whatever is there came from text, which still has it.
			e.Close()
			clearFiles(o.DBDir)
			if e, err = duckstore.OpenFiles(o.Input, o.Warn, fileOptions(o, true)); err != nil {
				return nil, err
			}
		}
		s := newStore(e, o)
		n, complete := s.importText(o, today)
		if complete && !e.Failed() {
			if err := e.SetMeta(duckstore.ImportedKey, "1"); err != nil {
				o.Warn("loghorn: DuckDB: " + firstLine(err))
			}
		}
		if n > 0 {
			o.Warn(fmt.Sprintf("loghorn: imported %d records from the text logs into DuckDB", n))
		}
		e.Archive(today)
		return s.ready(today), nil
	}
	e.Archive(today) // failures are warned about and retried next time
	return newStore(e, o).ready(today), nil
}

func archived(dir string) bool {
	entries, _ := os.ReadDir(filepath.Join(dir, duckstore.ArchiveDir))
	return len(entries) > 0
}

// clearFiles removes the database files and the archive.
func clearFiles(dir string) {
	for _, name := range duckstore.FileNames {
		for _, sfx := range []string{"", ".wal"} {
			os.Remove(filepath.Join(dir, name+sfx))
		}
	}
	os.RemoveAll(filepath.Join(dir, duckstore.ArchiveDir))
}

// ready continues today's ids after the stored ones.
func (s *Store) ready(today int) *Store {
	s.mu.Lock()
	s.seq[today] = s.e.MaxSeq(today)
	s.mu.Unlock()
	return s
}

// importText adds the retained days' text, each day with its own ids. It
// returns how many records it added and whether every file was read; an
// incomplete import leaves the files unmarked, to be done again.
func (s *Store) importText(o Options, today int) (n int, complete bool) {
	before := s.e.Count()
	complete = true
	for d := today - KeepDays; d <= today; d++ {
		if files := dayText(o.Dir, d); textBytes(files) > 0 {
			if err := s.load(files, d, o.Input); err != nil {
				o.Warn("loghorn: reading the text logs: " + firstLine(err) + "; the next start imports them again")
				complete = false
			}
		}
	}
	return s.e.Count() - before, complete
}

// openText reads the retained days' text into memory.
func openText(o Options, today int) (*Store, error) {
	e, err := duckstore.OpenMemory(o.Input, o.Warn)
	if err != nil {
		return nil, err
	}
	s := newStore(e, o)
	for d := today - KeepDays; d <= today; d++ {
		if files := dayText(o.Dir, d); textBytes(files) > 0 {
			if err := s.load(files, d, o.Input); err != nil {
				o.Warn("loghorn: reading the text logs: " + firstLine(err))
			}
		}
	}
	return s.ready(today), nil
}

// load reads text files of day into s, ids carrying that day. A day is
// read on its own, so its untimed records take times from that day only:
// leading ones the day's first time, and with none they stay untimed.
func (s *Store) load(paths []string, day int, in config.Input) error {
	s.mu.Lock()
	s.last, s.pending = time.Time{}, nil
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.last, s.pending = time.Time{}, nil
		s.mu.Unlock()
	}()
	p := adapter.New(in)
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		err = ingest.Stored(f, func(rec []byte) {
			l := bytes.Clone(rec)
			if r, ok := record.NormalizeSpan(l); ok {
				s.add(r, time.Time{}, day, true)
			} else if r, ok := record.Normalize(p.ParseLine(l), in); ok {
				s.add(r, time.Time{}, day, true)
			}
		})
		f.Close()
		if err != nil {
			return err
		}
	}
	s.Flush()
	return nil
}

// dayText lists a day's text files: logs, then spans; each the archive
// loghorn-YYYY-MM-DD<suffix>, then the current file when it was last
// written that day.
func dayText(dir string, day int) []string {
	var out []string
	name := dayName(day)
	for _, f := range []struct{ base, suffix string }{{logfile.LogBase, logfile.LogSuffix}, {logfile.SpansBase, logfile.SpansSuffix}} {
		arch := filepath.Join(dir, f.base+"-"+name+f.suffix)
		if fi, err := os.Stat(arch); err == nil && fi.Mode().IsRegular() {
			out = append(out, arch)
		}
		cur := filepath.Join(dir, f.base+f.suffix)
		if fi, err := os.Stat(cur); err == nil && fi.Mode().IsRegular() && DayNumber(fi.ModTime()) == day {
			out = append(out, cur)
		}
	}
	return out
}

func textBytes(paths []string) int64 {
	var n int64
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil {
			n += fi.Size()
		}
	}
	return n
}

func firstLine(err error) string {
	m := err.Error()
	if i := strings.IndexByte(m, '\n'); i >= 0 {
		m = m[:i]
	}
	return m
}

// dayName is a day number's date, YYYY-MM-DD, as the text archives name it.
func dayName(day int) string { return time.Unix(int64(day)*86400, 0).UTC().Format("2006-01-02") }
