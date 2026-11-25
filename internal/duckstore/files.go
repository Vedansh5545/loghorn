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
	// records are copied into memory and the files let go at once, so a
	// reader never keeps the owner out (DuckDB locks a file per process).
	Write bool
	// KeepDays is how many days before today archives are kept.
	KeepDays int
	Now      func() time.Time // nil: time.Now
}

// FileNames are the database files, logs then traces.
var FileNames = map[string]string{record.SourceLog: "logs.duckdb", record.SourceOtel: "traces.duckdb"}

var fileAliases = map[string]string{record.SourceLog: "logs_db", record.SourceOtel: "traces_db"}

// OpenFiles opens an engine over logs.duckdb and traces.duckdb in o.Dir (and
// the Parquet archives beside them). The writer creates missing files,
// retrying briefly while a reader copies them; a reader needs both to exist.
func OpenFiles(in config.Input, warn func(string), o FileOptions) (*Engine, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
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

// copyAndDetach copies a read-only file's records and tallies into the
// in-memory tables and detaches it.
func (e *Engine) copyAndDetach(p *part) error {
	defer e.exec("DETACH " + ident(p.alias))
	if err := e.exec("INSERT INTO " + e.mem[p.source].ref() + " SELECT " + columnList() + " FROM " + p.ref()); err != nil {
		return err
	}
	return e.exec("INSERT INTO " + e.mem[p.source].fieldsRef() + " SELECT * FROM " + p.fieldsRef())
}

func (e *Engine) metaRef() string { return ident(fileAliases[record.SourceLog]) + ".main._meta" }

// Meta reads a value the files keep about themselves.
func (e *Engine) Meta(key string) (string, bool) {
	if e.files == nil {
		return "", false
	}
	var v string
	if err := e.db.QueryRow("SELECT value FROM "+e.metaRef()+" WHERE key = ?", key).Scan(&v); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			e.warnOnce("loghorn: DuckDB: " + firstLine(err))
		}
		return "", false
	}
	return v, true
}

// SetMeta stores a value in the files (writer only).
func (e *Engine) SetMeta(key, value string) error {
	if e.files == nil || !e.files.Write {
		return errors.New("duckstore: SetMeta needs the writer")
	}
	e.wmu.Lock()
	defer e.wmu.Unlock()
	return e.exec("INSERT OR REPLACE INTO "+e.metaRef()+" VALUES (?, ?)", key, value)
}

// closeFiles checkpoints the database files at Close, so the next open
// needn't replay their write-ahead logs.
func (e *Engine) closeFiles() error {
	if e.files == nil || !e.files.Write {
		return nil
	}
	var errs []error
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
