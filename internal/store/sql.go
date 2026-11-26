package store

import (
	"context"
	"time"

	"github.com/jimbeveridge/loghorn/internal/duckstore"
	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/schema"
)

// SQLResult is the answer to a SQL query: its column names and at most the
// asked number of rows, as export values. Truncated is true when the query
// had more rows.
type SQLResult struct {
	Columns   []string
	Rows      [][]any
	Truncated bool
}

// SQLTimeout bounds one SQL query.
const SQLTimeout = 30 * time.Second

// SQL runs a read-only query over the table model of query.SQLHeader:
// logs."<stream>", spans."<stream>" and "*" of each, built from the stored
// records and the schema inferred now, and the loghorn.this_run_* macros
// for the latest run. Anything but one query over those tables is refused.
// At most max rows are read.
func (s *Store) SQL(ctx context.Context, q string, max int) (SQLResult, error) {
	tables, err := s.modelTables()
	if err != nil {
		return SQLResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, SQLTimeout)
	defer cancel()
	defer s.read()()
	start, runID := s.runStart, ""
	if id, t, ok := s.e.LatestRun(ctx); ok {
		start, runID = t, id
	}
	var res SQLResult
	err = s.e.WithModel(ctx, duckstore.ModelDDL(tables, start, runID), func() error {
		if err := duckstore.CheckReadOnly(ctx, s.e.DB(), q); err != nil {
			return err
		}
		rows, err := s.e.DB().QueryContext(ctx, q)
		if err != nil {
			return err
		}
		defer rows.Close()
		if res.Columns, err = rows.Columns(); err != nil {
			return err
		}
		for rows.Next() {
			if len(res.Rows) == max {
				res.Truncated = true
				break
			}
			vals := make([]any, len(res.Columns))
			ptrs := make([]any, len(vals))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			for i, v := range vals {
				vals[i] = duckstore.SQLValue(v)
			}
			res.Rows = append(res.Rows, vals)
		}
		return rows.Err()
	})
	return res, err
}

// modelTables is a table per stored stream and a "*" table per source, each
// with its stream's schema over every service.
func (s *Store) modelTables() ([]duckstore.ModelTable, error) {
	var out []duckstore.ModelTable
	add := func(src, stream string) error {
		inferred, err := s.Schema("*", src, stream)
		if err != nil {
			return err
		}
		out = append(out, duckstore.ModelTable{Source: src, Stream: stream, Fields: schema.Effective(inferred, nil).Fields})
		return nil
	}
	for _, src := range []string{record.SourceLog, record.SourceOtel} {
		if err := add(src, record.AllStreams); err != nil {
			return nil, err
		}
	}
	for _, c := range s.Streams() {
		if err := add(c.Source, c.Name); err != nil {
			return nil, err
		}
	}
	return out, nil
}
