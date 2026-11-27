package duckstore

import (
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
	for _, ddl := range []string{tableDDL(p.ref()), fieldsDDL(p.fieldsRef())} {
		if err := e.exec(ddl); err != nil {
			return err
		}
	}
	e.dest = p
	e.parts = append(e.parts, p)
	return nil
}

// ImportedFile, beside the database file, marks files that hold every
// record the retained days' text holds. It is a file of its own, not a row
// in loghorn.duckdb, so it can be cleared when that file can't be written.
const ImportedFile = FileName + ".imported"

// TakeImported reports whether dir's files are marked as holding every
// record, and clears the mark. Until a clean Close sets it again, a crash,
// a failed write or a session kept in memory leaves it cleared, so the next
// open imports the text again. A mark that can't be cleared counts as unset.
func TakeImported(dir string) bool {
	path := filepath.Join(dir, ImportedFile)
	if _, err := os.Stat(path); err != nil {
		return false
	}
	return os.Remove(path) == nil
}

// MarkComplete says the files hold every record from text, so a clean Close
// marks them (see TakeImported).
func (e *Engine) MarkComplete() {
	e.wmu.Lock()
	e.complete = true
	e.wmu.Unlock()
}

// closeFiles checkpoints the database file at Close, so the next open
// needn't replay its write-ahead log, and marks complete files as imported.
// Records this run kept out of the file (a full queue, a failed write)
// leave them unmarked, so the next open imports them again from text.
func (e *Engine) closeFiles() error {
	if e.files == nil {
		return nil
	}
	var errs []error
	e.mu.Lock()
	skipped := e.skipped
	e.mu.Unlock()
	if skipped > 0 {
		e.warn(fmt.Sprintf("loghorn: DuckDB skipped %d records this run; loghorn serve imports them again from the text logs when it next starts", skipped))
	}
	for _, p := range e.parts {
		if p.file {
			errs = append(errs, e.exec("CHECKPOINT "+ident(p.alias)))
		}
	}
	if e.complete && skipped == 0 && !e.failed && errors.Join(errs...) == nil {
		errs = append(errs, os.WriteFile(filepath.Join(e.files.Dir, ImportedFile), nil, 0o600))
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
