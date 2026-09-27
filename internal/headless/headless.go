// Package headless runs the loghorn engine as a stdin->stdout filter (no UI).
package headless

import (
	"io"

	"github.com/jimbeveridge/loghorn/internal/adapter"
	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/engine"
	"github.com/jimbeveridge/loghorn/internal/ingest"
)

// Run reads records from r and writes the original bytes of each important one
// (plus a newline) to w. Output is byte-for-byte faithful to the input, so a
// multi-line record prints across as many lines as it arrived on. If sink is
// non-nil it receives every record first, important or not — the log file
// keeps the whole stream. The slice passed to sink is only valid during the
// call. in decides both how the stream is framed and how a record's fields are
// read.
func Run(r io.Reader, w io.Writer, in config.Input, sink func(rec []byte)) error {
	return run(func(emit func([]byte)) error {
		return ingest.Records(r, in.Format, emit)
	}, w, in, sink)
}

// RunStored is Run for one of loghorn's own stored log files, framed by
// ingest.Stored rather than by in.Format: a stored day can mix producers, so
// no single framing fits it. in still decides how a record's fields are read.
// There is no sink, since a stored file is already recorded.
func RunStored(r io.Reader, w io.Writer, in config.Input) error {
	return run(func(emit func([]byte)) error {
		return ingest.Stored(r, emit)
	}, w, in, nil)
}

func run(frame func(emit func([]byte)) error, w io.Writer, in config.Input, sink func(rec []byte)) error {
	p := adapter.New(in)
	var writeErr error
	err := frame(func(rec []byte) {
		if sink != nil {
			sink(rec)
		}
		if writeErr != nil {
			return
		}
		e := p.ParseLine(rec)
		if !engine.IsImportant(e) {
			return
		}
		if _, err := w.Write(e.Raw); err != nil {
			writeErr = err
			return
		}
		if _, err := w.Write([]byte("\n")); err != nil {
			writeErr = err
		}
	})
	if err != nil {
		return err
	}
	return writeErr
}
