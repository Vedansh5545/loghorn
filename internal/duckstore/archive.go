package duckstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jimbeveridge/loghorn/internal/record"
)

// ArchiveDir is the directory beside the database files holding the
// Parquet archives of closed days: one file per signal per day.
const ArchiveDir = "duckdb-archive"

var archiveKinds = map[string]string{record.SourceLog: "logs", record.SourceOtel: "traces"}

type archiveFile struct {
	source string
	day    int
	path   string
}

type archiveState struct {
	files   []archiveFile
	lastDay int // the latest day written (writer); 0 until Archive runs
}

// dayOf is the day number in an _id.
func dayOf(id int) int { return id >> 32 }

// dayName is a day number's date, YYYY-MM-DD.
func dayName(day int) string { return time.Unix(int64(day)*86400, 0).UTC().Format("2006-01-02") }

func (e *Engine) archiveDir() string { return filepath.Join(e.files.Dir, ArchiveDir) }

func (e *Engine) archivePath(source string, day int) string {
	return filepath.Join(e.archiveDir(), archiveKinds[source]+"-"+dayName(day)+".parquet")
}

// loadArchives lists the archives in the files' directory. The writer
// removes exports a crash left unfinished.
func (e *Engine) loadArchives() error {
	entries, err := os.ReadDir(e.archiveDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	e.arch.files = nil
	for _, en := range entries {
		name := en.Name()
		if strings.HasSuffix(name, ".parquet.tmp") && e.files.Write {
			os.Remove(filepath.Join(e.archiveDir(), name))
			continue
		}
		for src, kind := range archiveKinds {
			rest, ok := strings.CutPrefix(name, kind+"-")
			if !ok || !strings.HasSuffix(rest, ".parquet") {
				continue
			}
			d, err := time.Parse("2006-01-02", strings.TrimSuffix(rest, ".parquet"))
			if err != nil {
				continue
			}
			e.arch.files = append(e.arch.files, archiveFile{src, int(d.Unix() / 86400), filepath.Join(e.archiveDir(), name)})
		}
	}
	sort.Slice(e.arch.files, func(i, j int) bool { return e.arch.files[i].path < e.arch.files[j].path })
	return nil
}

// archiveSelect reads Parquet archives with the tables' columns.
func archiveSelect(paths []string) string {
	q := make([]string, len(paths))
	for i, p := range paths {
		q[i] = lit(p)
	}
	cols := strings.Replace(columnList(), "attributes", "CAST(attributes AS JSON) AS attributes", 1)
	return "SELECT " + cols + " FROM read_parquet([" + strings.Join(q, ", ") + "], union_by_name = true)"
}

// archiveSelects reads the archives for all_records. An archive whose day
// still has rows in a hot table (a crash between publishing it and deleting
// them) leaves out the rows it shares with them, so nothing is read twice.
func (e *Engine) archiveSelects() []string {
	var out []string
	for _, src := range []string{record.SourceLog, record.SourceOtel} {
		var plain, overlap []string
		hot := e.hotDays(src)
		for _, f := range e.arch.files {
			switch {
			case f.source != src:
			case hot[f.day]:
				overlap = append(overlap, f.path)
			default:
				plain = append(plain, f.path)
			}
		}
		if len(plain) > 0 {
			out = append(out, archiveSelect(plain))
		}
		if len(overlap) > 0 {
			out = append(out, archiveSelect(overlap)+" WHERE _id NOT IN ("+e.hotIDs(src)+")")
		}
	}
	return out
}

// hotIDs selects the ids of source in the tables (files and memory).
func (e *Engine) hotIDs(src string) string {
	var q []string
	for _, p := range e.parts {
		if p.source == src {
			q = append(q, "SELECT _id FROM "+p.ref())
		}
	}
	return strings.Join(q, " UNION ALL ")
}

// hotDays are the days of source with rows in a table.
func (e *Engine) hotDays(src string) map[int]bool {
	out := map[int]bool{}
	rows, err := e.wc.QueryContext(bg, "SELECT DISTINCT _id >> 32 FROM ("+e.hotIDs(src)+")")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var d int
		if rows.Scan(&d) == nil {
			out[d] = true
		}
	}
	return out
}

// Archive moves every closed day (before today) from the database files to
// Parquet, then deletes archives older than today − KeepDays (writer only).
// A day whose export fails or doesn't validate stays in the files, with a
// warning, and is tried again next time.
func (e *Engine) Archive(today int) error {
	if e.files == nil || !e.files.Write {
		return nil
	}
	e.wmu.Lock()
	defer e.wmu.Unlock()
	return e.archive(today)
}

// archiveAtMidnight archives the closed days when batch reaches a later day
// than any written before. Writer side, after the batch is written.
func (e *Engine) archiveAtMidnight(batch []record.Record) {
	if e.files == nil || !e.files.Write || e.arch.lastDay == 0 {
		return
	}
	d := 0
	for _, r := range batch {
		d = max(d, dayOf(r.Seq))
	}
	if d > e.arch.lastDay {
		e.archive(d)
	}
}

func (e *Engine) archive(today int) error {
	e.arch.lastDay = max(e.arch.lastDay, today)
	if e.failed {
		return nil // the files aren't being written: leave them be
	}
	if err := os.MkdirAll(e.archiveDir(), 0o700); err != nil {
		e.warnOnce("loghorn: DuckDB archive: " + err.Error())
		return err
	}
	var errs []error
	for _, src := range []string{record.SourceLog, record.SourceOtel} {
		p := e.dest[src]
		if !p.file {
			continue
		}
		for _, d := range e.closedDays(p, today) {
			if err := e.archiveDay(p, d); err != nil {
				e.warnOnce(fmt.Sprintf("loghorn: DuckDB archive of %s %s failed (%s); its records stay in %s", archiveKinds[src], dayName(d), firstLine(err), FileNames[src]))
				errs = append(errs, err)
			}
		}
	}
	e.schemaMu.Lock()
	defer e.schemaMu.Unlock()
	e.expireArchives(today)
	if err := e.rebuildViews(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (e *Engine) closedDays(p *part, today int) []int {
	rows, err := e.wc.QueryContext(bg, "SELECT DISTINCT _id >> 32 AS d FROM "+p.ref()+" WHERE _id < ? ORDER BY d", int64(today)<<32)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var d int
		if rows.Scan(&d) == nil {
			out = append(out, d)
		}
	}
	return out
}

// archiveDay exports one day of p, with any rows of an earlier archive of
// that day it doesn't have, to a temporary file; checks the file holds
// exactly those rows; publishes it; and deletes the day's rows from p.
func (e *Engine) archiveDay(p *part, day int) error {
	path := e.archivePath(p.source, day)
	tmp := path + ".tmp"
	os.Remove(tmp)
	lo, hi := int64(day)<<32, int64(day+1)<<32
	src := fmt.Sprintf("SELECT %s FROM %s WHERE _id >= %d AND _id < %d", columnList(), p.ref(), lo, hi)
	if _, err := os.Stat(path); err == nil {
		src += " UNION ALL " + archiveSelect([]string{path}) + " WHERE _id NOT IN (SELECT _id FROM " + p.ref() + ")"
	}
	err := e.exec("COPY (" + src + " ORDER BY _id) TO " + lit(tmp) + " (FORMAT parquet, COMPRESSION zstd)")
	if err == nil {
		err = e.validate(src, tmp)
	}
	if err == nil {
		err = syncFile(tmp)
	}
	if err == nil {
		os.Chmod(tmp, 0o600)
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	// From here the archive holds the day; readers wait while the rows
	// leave the file and the view starts reading the archive whole.
	e.schemaMu.Lock()
	defer e.schemaMu.Unlock()
	if !e.hasArchive(path) {
		e.arch.files = append(e.arch.files, archiveFile{p.source, day, path})
	}
	if err := e.exec("DELETE FROM "+p.ref()+" WHERE _id >= ? AND _id < ?", lo, hi); err != nil {
		return err
	}
	return e.rebuildViews()
}

func (e *Engine) hasArchive(path string) bool {
	for _, f := range e.arch.files {
		if f.path == path {
			return true
		}
	}
	return false
}

// validate compares the rows of query src with the Parquet file: the count,
// the ids, and hashes of every _raw and attributes.
func (e *Engine) validate(src, file string) error {
	if e.failArchive {
		return errors.New("validation refused (test)")
	}
	agg := "SELECT count(*), min(_id), max(_id), sum(_id), bit_xor(hash(_raw)), bit_xor(hash(CAST(attributes AS VARCHAR))) FROM "
	var a, b [6]any
	if err := e.wc.QueryRowContext(bg, agg+"("+src+")").Scan(&a[0], &a[1], &a[2], &a[3], &a[4], &a[5]); err != nil {
		return err
	}
	if err := e.wc.QueryRowContext(bg, agg+"("+archiveSelect([]string{file})+")").Scan(&b[0], &b[1], &b[2], &b[3], &b[4], &b[5]); err != nil {
		return err
	}
	if fmt.Sprint(a) != fmt.Sprint(b) {
		return fmt.Errorf("archive doesn't match its rows: %v, want %v", b, a)
	}
	return nil
}

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// expireArchives deletes archives and tallies of days before today −
// KeepDays. Called with schemaMu held: the view is rebuilt next.
func (e *Engine) expireArchives(today int) {
	cutoff := today - e.files.KeepDays
	keep := e.arch.files[:0]
	for _, f := range e.arch.files {
		if f.day < cutoff {
			os.Remove(f.path)
			continue
		}
		keep = append(keep, f)
	}
	e.arch.files = keep
	for _, p := range e.parts {
		if p.file && p.writable {
			if err := e.exec("DELETE FROM "+p.fieldsRef()+" WHERE day < ?", cutoff); err != nil {
				e.warnOnce("loghorn: DuckDB: " + firstLine(err))
			}
		}
	}
}
