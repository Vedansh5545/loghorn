package duckstore

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jimbeveridge/loghorn/internal/config"
)

// FileOptions opens the database file in a directory. The engine is the
// file's one owner (loghorn serve, holding the data folder's lock): it
// creates, writes and archives the file, and keeps it open.
type FileOptions struct {
	Dir string
	// KeepDays is how many days before today archives are kept.
	KeepDays int
}

// FileName is the database file: the records table (logs and spans), the
// field tallies and the marks.
const FileName = "loghorn.duckdb"

const fileAlias = "loghorn_db"

// OpenFiles opens an engine over loghorn.duckdb in o.Dir (and the Parquet
// archives beside it), creating a missing file.
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
	if err := e.rebuildViews(); err != nil {
		e.closeDB()
		return nil, err
	}
	if err := e.harden(); err != nil {
		e.closeDB()
		return nil, err
	}
	e.start()
	return e, nil
}

func (e *Engine) attachFiles() error {
	o := e.files
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(o.Dir, FileName)
	if err := e.exec("ATTACH " + lit(path) + " AS " + ident(fileAlias)); err != nil {
		return fmt.Errorf("%s: %w", FileName, errors.New(firstLine(err)))
	}
	p := &part{alias: fileAlias, writable: true, file: true}
	os.Chmod(path, 0o600)
	for _, ddl := range []string{tableDDL(p.ref()), fieldsDDL(p.fieldsRef()),
		"CREATE TABLE IF NOT EXISTS " + e.metaRef() + " (key VARCHAR PRIMARY KEY, value VARCHAR)"} {
		if err := e.exec(ddl); err != nil {
			return err
		}
	}
	e.dest = p
	e.parts = append(e.parts, p)
	return nil
}

func (e *Engine) metaRef() string { return ident(fileAlias) + ".main._meta" }

// Meta reads a value the files keep about themselves; "" when unset.
func (e *Engine) Meta(key string) (string, error) {
	if e.files == nil {
		return "", nil
	}
	var v string
	err := e.db.QueryRow("SELECT value FROM "+e.metaRef()+" WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetMeta stores a value in the files.
func (e *Engine) SetMeta(key, value string) error {
	if e.files == nil {
		return errors.New("duckstore: SetMeta needs the files")
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

// closeFiles checkpoints the database file at Close, so the next open
// needn't replay their write-ahead logs. Records this run kept out of the
// files (a full queue, a failed write) clear the imported mark, so the
// next import brings them back from text.
func (e *Engine) closeFiles() error {
	if e.files == nil {
		return nil
	}
	var errs []error
	e.mu.Lock()
	skipped := e.skipped
	e.mu.Unlock()
	if skipped > 0 || e.failed {
		if skipped > 0 {
			e.warn(fmt.Sprintf("loghorn: DuckDB skipped %d records this run; loghorn serve imports them again from the text logs when it next starts", skipped))
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

// ownerOnly makes the file's write-ahead log 0600 like the file. DuckDB
// creates a WAL at the first write after a checkpoint, with the process
// umask, so this runs after every batch.
func (e *Engine) ownerOnly() {
	if e.files == nil {
		return
	}
	wal := filepath.Join(e.files.Dir, FileName+".wal")
	if fi, err := os.Stat(wal); err == nil && fi.Mode().Perm() != 0o600 {
		os.Chmod(wal, 0o600)
	}
}
