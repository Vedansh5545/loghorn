package duckstore

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/record"
)

// FileOptions opens the database files in a directory.
type FileOptions struct {
	Dir string
	// Write is true in the one process that owns the files: it creates,
	// writes and archives them, and keeps them open. Otherwise the files'
	// and archives' records are copied into memory and nothing is held
	// open, so a reader never keeps the owner out (DuckDB locks a file per
	// process) and outlives archives the owner later replaces or deletes.
	Write bool
	// KeepDays is how many days before today archives are kept.
	KeepDays int
}

// FileNames are the database files, logs then traces.
var FileNames = map[string]string{record.SourceLog: "logs.duckdb", record.SourceOtel: "traces.duckdb"}

var fileAliases = map[string]string{record.SourceLog: "logs_db", record.SourceOtel: "traces_db"}

// OpenFiles opens an engine over logs.duckdb and traces.duckdb in o.Dir (and
// the Parquet archives beside them). The writer creates missing files,
// retrying briefly while a reader copies them; a reader needs both to exist.
func OpenFiles(in config.Input, warn func(string), o FileOptions) (*Engine, error) {
	e, err := open(in, warn)
	if err != nil {
		return nil, err
	}
	e.files = &o
	if err := e.attachFiles(); err != nil {
		e.closeDB()
		return nil, err
	}
	if err := e.loadArchives(); err != nil {
		e.closeDB()
		return nil, err
	}
	if !o.Write {
		if err := e.copyArchives(); err != nil {
			e.closeDB()
			return nil, err
		}
	}
	if err := e.rebuildViews(); err != nil {
		e.closeDB()
		return nil, err
	}
	e.start()
	return e, nil
}

func (e *Engine) attachFiles() error {
	o := e.files
	if o.Write {
		if err := os.MkdirAll(o.Dir, 0o700); err != nil {
			return err
		}
	}
	for _, src := range []string{record.SourceLog, record.SourceOtel} {
		path := filepath.Join(o.Dir, FileNames[src])
		q := "ATTACH " + lit(path) + " AS " + ident(fileAliases[src])
		if !o.Write {
			if _, err := os.Stat(path); err != nil {
				return err
			}
			q += " (READ_ONLY)"
		}
		if err := e.attachRetry(q, o.Write); err != nil {
			return fmt.Errorf("%s: %w", FileNames[src], errors.New(firstLine(err)))
		}
		p := &part{alias: fileAliases[src], source: src, writable: o.Write, file: true}
		if !o.Write {
			if err := e.copyAndDetach(p); err != nil {
				return err
			}
			continue
		}
		os.Chmod(path, 0o600)
		for _, ddl := range []string{tableDDL(p.ref()), fieldsDDL(p.fieldsRef())} {
			if err := e.exec(ddl); err != nil {
				return err
			}
		}
		if src == record.SourceLog {
			if err := e.exec("CREATE TABLE IF NOT EXISTS " + e.metaRef() + " (key VARCHAR PRIMARY KEY, value VARCHAR)"); err != nil {
				return err
			}
		}
		e.dest[src] = p
		e.parts = append(e.parts, p)
	}
	return nil
}

// attachRetry attaches, retrying the writer's attach for up to two seconds
// while a reader holds the file to copy it.
func (e *Engine) attachRetry(q string, write bool) error {
	err := e.exec(q)
	for i := 0; err != nil && write && i < 10; i++ {
		time.Sleep(200 * time.Millisecond)
		err = e.exec(q)
	}
	return err
}

// copyAndDetach copies a read-only file's records, tallies and (for logs)
// marks into memory and detaches it.
func (e *Engine) copyAndDetach(p *part) error {
	defer e.exec("DETACH " + ident(p.alias))
	if err := e.exec("INSERT INTO " + e.mem[p.source].ref() + " SELECT " + columnList() + " FROM " + p.ref()); err != nil {
		return err
	}
	if err := e.exec("INSERT INTO " + e.mem[p.source].fieldsRef() + " SELECT * FROM " + p.fieldsRef()); err != nil {
		return err
	}
	if p.source != record.SourceLog {
		return nil
	}
	e.meta = map[string]string{}
	rows, err := e.wc.QueryContext(bg, "SELECT key, value FROM "+e.metaRef())
	if err != nil {
		return nil // files from before marks existed: none
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return err
		}
		e.meta[k] = v
	}
	return rows.Err()
}

// copyArchives copies a reader's archives into memory, leaving out rows the
// files' copy already has, and forgets the archive files.
func (e *Engine) copyArchives() error {
	for _, src := range []string{record.SourceLog, record.SourceOtel} {
		var paths []string
		for _, f := range e.arch.files {
			if f.source == src {
				paths = append(paths, f.path)
			}
		}
		if len(paths) == 0 {
			continue
		}
		m := e.mem[src].ref()
		if err := e.exec("INSERT INTO " + m + " " + archiveSelect(paths) + " WHERE _id NOT IN (SELECT _id FROM " + m + ")"); err != nil {
			return err
		}
	}
	e.arch.files = nil
	return nil
}

func (e *Engine) metaRef() string { return ident(fileAliases[record.SourceLog]) + ".main._meta" }

// Meta reads a value the files keep about themselves; "" when unset.
func (e *Engine) Meta(key string) (string, error) {
	switch {
	case e.files == nil:
		return "", nil
	case !e.files.Write:
		return e.meta[key], nil
	}
	var v string
	err := e.db.QueryRow("SELECT value FROM "+e.metaRef()+" WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetMeta stores a value in the files (writer only).
func (e *Engine) SetMeta(key, value string) error {
	if e.files == nil || !e.files.Write {
		return errors.New("duckstore: SetMeta needs the writer")
	}
	e.wmu.Lock()
	defer e.wmu.Unlock()
	return e.setMeta(key, value)
}

func (e *Engine) setMeta(key, value string) error {
	return e.exec("INSERT OR REPLACE INTO "+e.metaRef()+" VALUES (?, ?)", key, value)
}

// ImportedKey marks files whose records all came in: from text once, then
// from runs that wrote every record. Skipped or failed writes clear it at
// Close, so the next import brings those records back from text.
const ImportedKey = "imported"

// closeFiles checkpoints the database files at Close, so the next open
// needn't replay their write-ahead logs. Records this run kept out of the
// files (a full queue, a failed write) clear the imported mark, so the
// next import brings them back from text.
func (e *Engine) closeFiles() error {
	if e.files == nil || !e.files.Write {
		return nil
	}
	var errs []error
	e.mu.Lock()
	skipped := e.skipped
	e.mu.Unlock()
	if skipped > 0 || e.failed {
		if skipped > 0 {
			e.warn(fmt.Sprintf("loghorn: DuckDB skipped %d records this run; the next -web run imports them again from the text logs", skipped))
		}
		errs = append(errs, e.setMeta(ImportedKey, "0"))
	}
	for _, p := range e.parts {
		if p.file {
			errs = append(errs, e.exec("CHECKPOINT "+ident(p.alias)))
		}
	}
	return errors.Join(errs...)
}

// ownerOnly makes the files' write-ahead logs 0600 like the files. DuckDB
// creates a WAL at the first write after a checkpoint, with the process
// umask, so this runs after every batch.
func (e *Engine) ownerOnly() {
	if e.files == nil || !e.files.Write {
		return
	}
	for _, name := range FileNames {
		wal := filepath.Join(e.files.Dir, name+".wal")
		if fi, err := os.Stat(wal); err == nil && fi.Mode().Perm() != 0o600 {
			os.Chmod(wal, 0o600)
		}
	}
}
