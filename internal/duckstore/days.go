package duckstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/record"
)

// DayOptions configures per-day database files in a .loghorn directory.
type DayOptions struct {
	Dir string
	// Write is true in the process holding loghorn.lock: it creates, stamps,
	// rolls over, rebuilds and expires day files. Others only read them.
	Write bool
	// KeepDays is how many days before today are kept.
	KeepDays int
	// TextBytes is the current size of a day's text files, recorded when a
	// day is closed cleanly (D-018).
	TextBytes func(day int) int64
}

// Stamp is what a day pair records about itself in _loghorn._day.
type Stamp struct {
	Clean     bool // closed by its writer, not left open by a crash
	Dirty     bool // a write was skipped or failed: rebuild from text
	TextBytes int64
}

// DayName is a day number's local date, YYYY-MM-DD.
func DayName(day int) string { return time.Unix(int64(day)*86400, 0).UTC().Format("2006-01-02") }

// OpenDays opens an engine for day files in o.Dir. Nothing is attached yet:
// the caller attaches, builds or loads each retained day. Records of a day
// with no writable pair go to memory.
func OpenDays(in config.Input, warn func(string), o DayOptions) (*Engine, error) {
	e, err := open(in, warn)
	if err != nil {
		return nil, err
	}
	if e.mem, err = e.memPair(); err != nil {
		e.closeDB()
		return nil, err
	}
	if o.TextBytes == nil {
		o.TextBytes = func(int) int64 { return 0 }
	}
	e.days = &o
	e.route = map[int]*pair{}
	e.pairFor = e.routeFor
	e.onFail = e.fileFailed
	if err := e.rebuildViews(); err != nil {
		e.closeDB()
		return nil, err
	}
	e.start()
	return e, nil
}

func (e *Engine) dayPath(kind string, day int, suffix string) string {
	return filepath.Join(e.days.Dir, kind+"-"+DayName(day)+".duckdb"+suffix)
}

// routeFor is the pair a record of day goes to. In the writer, a day past
// the open one rolls over to it (midnight).
func (e *Engine) routeFor(day int) (*pair, error) {
	if p := e.route[day]; p != nil {
		return p, nil
	}
	if e.days.Write && e.today != nil && day > e.today.logs.day && !e.failed {
		if err := e.rollover(day); err != nil {
			return nil, err
		}
		return e.route[day], nil
	}
	return e.mem, nil
}

// ReadStamp reads a day pair's stamp, attaching it read-only for a moment.
// ok is false when either file is missing or can't be read (for example
// while another process writes it).
func (e *Engine) ReadStamp(day int) (Stamp, bool) {
	e.wmu.Lock()
	defer e.wmu.Unlock()
	var st Stamp
	for i, kind := range []string{"logs", "traces"} {
		path := e.dayPath(kind, day, "")
		if _, err := os.Stat(path); err != nil {
			return Stamp{}, false
		}
		alias := "chk_" + kind
		if err := e.exec("ATTACH " + lit(path) + " AS " + ident(alias) + " (READ_ONLY)"); err != nil {
			return Stamp{}, false
		}
		var s Stamp
		err := e.wc.QueryRowContext(bg, "SELECT clean, dirty, text_bytes FROM "+ident(alias)+"."+metaSchema+"._day").Scan(&s.Clean, &s.Dirty, &s.TextBytes)
		e.exec("DETACH " + ident(alias))
		if err != nil {
			return Stamp{}, false
		}
		if i == 0 {
			st = s
		} else {
			st.Clean = st.Clean && s.Clean
			st.Dirty = st.Dirty || s.Dirty
		}
	}
	return st, true
}

// AttachDay attaches a day pair. A writable one becomes the open day that
// new records go to; it is created if missing and stamped unclean until
// closed. A read-only one must exist.
func (e *Engine) AttachDay(day int, writable bool) error {
	e.wmu.Lock()
	defer e.wmu.Unlock()
	p, err := e.attachPair(day, "", writable)
	if err != nil {
		return err
	}
	if writable {
		if err := e.stamp(p, Stamp{}); err != nil {
			return err
		}
		e.route[day] = p
		e.today = p
	}
	return e.rebuildViews()
}

func (e *Engine) attachPair(day int, suffix string, writable bool) (*pair, error) {
	prefix := ""
	if suffix != "" {
		prefix = "b"
	}
	var cats [2]*catalog
	for i, kind := range []string{"logs", "traces"} {
		path := e.dayPath(kind, day, suffix)
		if !writable {
			if _, err := os.Stat(path); err != nil {
				if i == 1 {
					e.detach(cats[0])
				}
				return nil, err
			}
		}
		alias := prefix + kind[:1] + DayName(day)
		src := record.SourceLog
		if kind == "traces" {
			src = record.SourceOtel
		}
		c, err := e.attach(alias, path, src, day, writable)
		if err != nil {
			if i == 1 {
				e.detach(cats[0])
			}
			return nil, err
		}
		if writable {
			os.Chmod(path, 0o600)
		}
		cats[i] = c
	}
	return &pair{logs: cats[0], traces: cats[1]}, nil
}

func (e *Engine) stamp(p *pair, st Stamp) error {
	for _, c := range []*catalog{p.logs, p.traces} {
		if err := e.exec("DELETE FROM " + c.meta("_day")); err != nil {
			return err
		}
		if err := e.exec("INSERT INTO "+c.meta("_day")+" VALUES (?, ?, ?, ?)", c.day, st.Clean, st.Dirty, st.TextBytes); err != nil {
			return err
		}
		if err := e.exec("CHECKPOINT " + ident(c.alias)); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) detachPair(p *pair) {
	for _, c := range []*catalog{p.logs, p.traces} {
		if err := e.detach(c); err != nil {
			e.warnOnce("loghorn: DuckDB: " + err.Error())
		}
	}
}

// closeDay stamps the open day clean (or dirty) and reattaches it
// read-only.
func (e *Engine) closeDay(p *pair, reattach bool) error {
	day := p.logs.day
	st := Stamp{Clean: true, Dirty: e.skippedDay(day) || e.failed, TextBytes: e.days.TextBytes(day)}
	err := e.stamp(p, st)
	e.detachPair(p)
	delete(e.route, day)
	if e.today == p {
		e.today = nil
	}
	if err != nil {
		return err
	}
	if reattach {
		_, err = e.attachPair(day, "", false)
	}
	return err
}

// MarkDirty makes day's pair close dirty, so the next writer rebuilds it
// from text, as after a skipped write.
func (e *Engine) MarkDirty(day int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.skipped[day] = true
}

func (e *Engine) skippedDay(day int) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.skipped[day]
}

// rollover closes the open day and opens day, then expires old days.
func (e *Engine) rollover(day int) error {
	if err := e.closeDay(e.today, true); err != nil {
		return err
	}
	p, err := e.attachPair(day, "", true)
	if err != nil {
		return err
	}
	if err := e.stamp(p, Stamp{}); err != nil {
		return err
	}
	e.route[day], e.today = p, p
	e.expire(day)
	return e.rebuildViews()
}

// Expire deletes day pairs older than today − KeepDays (writer only).
func (e *Engine) Expire(today int) {
	e.wmu.Lock()
	defer e.wmu.Unlock()
	e.expire(today)
	if err := e.rebuildViews(); err != nil {
		e.warnOnce("loghorn: DuckDB views: " + err.Error())
	}
}

func (e *Engine) expire(today int) {
	if !e.days.Write {
		return
	}
	cutoff := today - e.days.KeepDays
	for _, c := range append([]*catalog(nil), e.cats...) {
		if c.day >= 0 && c.day < cutoff {
			e.detach(c)
		}
	}
	entries, err := os.ReadDir(e.days.Dir)
	if err != nil {
		return
	}
	for _, en := range entries {
		name := en.Name()
		for _, kind := range []string{"logs-", "traces-"} {
			rest, ok := strings.CutPrefix(name, kind)
			if !ok || len(rest) < 10 {
				continue
			}
			d, err := time.Parse("2006-01-02", rest[:10])
			if err != nil || !strings.HasPrefix(rest[10:], ".duckdb") {
				continue
			}
			if int(d.Unix()/86400) < cutoff {
				os.Remove(filepath.Join(e.days.Dir, name))
			}
		}
	}
}

// BuildDay writes a day pair from scratch: load adds the day's records (with
// that day's ids), then the pair is stamped clean, renamed into place, and
// detached. Writer only.
func (e *Engine) BuildDay(day int, load func() error) error {
	if !e.days.Write {
		return errors.New("duckstore: BuildDay needs the writer")
	}
	e.wmu.Lock()
	for _, kind := range []string{"logs", "traces"} {
		for _, sfx := range []string{".tmp", ".tmp.wal"} {
			os.Remove(e.dayPath(kind, day, sfx))
		}
	}
	p, err := e.attachPair(day, ".tmp", true)
	if err != nil {
		e.wmu.Unlock()
		return err
	}
	e.route[day] = p
	e.wmu.Unlock()

	lerr := load()
	e.Sync(24 * time.Hour)

	e.wmu.Lock()
	defer e.wmu.Unlock()
	st := Stamp{Clean: lerr == nil, Dirty: e.skippedDay(day) || e.failed, TextBytes: e.days.TextBytes(day)}
	serr := e.stamp(p, st)
	e.detachPair(p)
	delete(e.route, day)
	if err := errors.Join(lerr, serr); err != nil {
		return err
	}
	for _, kind := range []string{"logs", "traces"} {
		final := e.dayPath(kind, day, "")
		os.Remove(final + ".wal")
		if err := os.Rename(e.dayPath(kind, day, ".tmp"), final); err != nil {
			return err
		}
	}
	return e.rebuildViews()
}

// MaxSeq is the highest per-day sequence stored for day, or 0.
func (e *Engine) MaxSeq(day int) int {
	var id *int64
	lo := int64(day) << 32
	if err := e.db.QueryRow("SELECT max(_id) FROM "+sessionSchema+"all_records WHERE _id >= ? AND _id < ?", lo, lo+1<<32).Scan(&id); err != nil || id == nil {
		return 0
	}
	return int(*id - lo)
}

// fileFailed handles the first failed file write: warn once, and route the
// rest of the session to memory. Closing stamps the day dirty, so it is
// rebuilt from text next time.
func (e *Engine) fileFailed(err error) {
	if e.failed {
		return
	}
	e.failed = true
	e.warn(fmt.Sprintf("loghorn: DuckDB write failed (%v); keeping this run's records in memory, the day is rebuilt from text next time", firstLine(err)))
}

func firstLine(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// closeDays stamps the open day at Close.
func (e *Engine) closeDays() error {
	e.wmu.Lock()
	defer e.wmu.Unlock()
	if e.today == nil {
		return nil
	}
	return e.closeDay(e.today, false)
}
