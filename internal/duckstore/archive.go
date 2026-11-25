package duckstore

import "github.com/jimbeveridge/loghorn/internal/record"

// ArchiveDir is the directory beside the database files holding the
// Parquet archives of closed days.
const ArchiveDir = "duckdb-archive"

type archiveState struct{}

// loadArchives finds the archives in the files' directory (none yet).
func (e *Engine) loadArchives() error { return nil }

// archiveSelects reads the Parquet archives (none yet).
func (e *Engine) archiveSelects() []string { return nil }

// archiveAtMidnight archives the closed day when batch starts a later one.
func (e *Engine) archiveAtMidnight(batch []record.Record) {}

// dayOf is the day number in an _id.
func dayOf(id int) int { return id >> 32 }
