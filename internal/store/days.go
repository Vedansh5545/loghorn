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

// Options opens a store over the day files in a .loghorn directory.
type Options struct {
	Mode     Mode
	RunStart time.Time
	Input    config.Input
	Dir      string // the .loghorn directory
	// Write is true in the process holding loghorn.lock (with or without
	// -web): it writes, rebuilds and expires the day files.
	Write bool
	Now   func() time.Time // nil: time.Now
	Warn  func(string)     // nil: print to stderr
}

// Open opens a store over Dir's retained days (today and KeepDays before).
//
//   - Write: each day whose DuckDB pair is missing or in doubt (D-018) and
//     has text files is rebuilt from them first; today's pair is opened for
//     writing and the others read-only.
//   - Live without Write: earlier days are read from their pairs, today
//     from its text files into memory, with this run's records.
//   - Otherwise (stored days): each day is read from its pair, or from its
//     text files into memory when the pair can't be used.
func Open(o Options) (*Store, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Warn == nil {
		o.Warn = func(m string) { fmt.Fprintln(os.Stderr, m) }
	}
	e, err := duckstore.OpenDays(o.Input, o.Warn, duckstore.DayOptions{
		Dir: o.Dir, Write: o.Write, KeepDays: KeepDays,
		TextBytes: func(d int) int64 { return textBytes(dayText(o.Dir, d)) },
	})
	if err != nil {
		return nil, err
	}
	s := wrap(e, o.Mode, o.RunStart)
	s.now = o.Now
	today := DayNumber(o.Now())
	if o.Write {
		e.Expire(today)
	}
	for d := today - KeepDays; d <= today; d++ {
		files := dayText(o.Dir, d)
		size := textBytes(files)
		if size == 0 {
			files = nil // empty files (a fresh loghorn.log) hold nothing to build
		}
		st, stamped := e.ReadStamp(d)
		valid := stamped && st.Clean && !st.Dirty && (len(files) == 0 || st.TextBytes == size)
		switch {
		case o.Write:
			if !valid && len(files) > 0 {
				o.Warn(fmt.Sprintf("loghorn: building DuckDB for %s from its text logs", duckstore.DayName(d)))
				if err := e.BuildDay(d, func() error { return s.load(files, d, o.Input) }); err != nil {
					o.Warn("loghorn: DuckDB: " + firstLine(err))
					s.load(files, d, o.Input) // read it from memory this run
					continue
				}
				stamped = true
			}
			if d == today {
				if err := e.AttachDay(d, true); err != nil {
					// Today stays in memory this run; its text keeps growing,
					// so the pair is rebuilt at the next start.
					o.Warn("loghorn: DuckDB: can't open today's database (" + firstLine(err) + "); keeping today in memory")
					s.load(files, d, o.Input)
				}
			} else if stamped {
				if err := e.AttachDay(d, false); err != nil {
					o.Warn("loghorn: DuckDB: " + err.Error())
				}
			}
		case d == today:
			// A writer has, or may soon want, today's pair open for writing.
			s.load(files, d, o.Input)
		default:
			if valid && e.AttachDay(d, false) == nil {
				continue
			}
			s.load(files, d, o.Input)
		}
	}
	if o.Write {
		s.mu.Lock()
		s.seq[today] = e.MaxSeq(today)
		s.mu.Unlock()
	}
	return s, nil
}

// load reads text files of day into s, ids carrying that day.
func (s *Store) load(paths []string, day int, in config.Input) error {
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
	name := duckstore.DayName(day)
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
