// Package incoming holds the runs loghorn run recorded while serve was down:
// one folder per run under <data>/incoming/<run id>/, with the run's records
// (loghorn.log), its spans (loghorn.traces.jsonl) and run.json describing it.
// A run holds its folder's lock while it writes, so serve adopts a folder
// only once its run has ended, and never takes the data folder's own lock.
package incoming

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/ingest"
	"github.com/jimbeveridge/loghorn/internal/logfile"
)

// Dir is the folder under the data folder that holds incoming runs.
const Dir = "incoming"

const metaName = "run.json"

// Meta describes an incoming run.
type Meta struct {
	RunID   string       `json:"runId"`
	Service string       `json:"service"`
	Input   config.Input `json:"input"`
	Started time.Time    `json:"started"`
}

// Create makes the run's folder, writes run.json and opens the folder's log
// and span writers. The log writer holds the folder's lock until it closes.
func Create(dataDir string, m Meta) (logs, spans *logfile.Writer, err error) {
	dir := filepath.Join(dataDir, Dir, m.RunID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, metaName), b, 0o600); err != nil {
		return nil, nil, err
	}
	logs, err = logfile.Open(dir, time.Now, time.Local)
	if err != nil {
		return nil, nil, err
	}
	spans, err = logs.Companion(logfile.SpansBase, logfile.SpansSuffix)
	if err != nil {
		logs.Close()
		return nil, nil, err
	}
	return logs, spans, nil
}

// Pending is an incoming run. Locked means its run is still writing.
type Pending struct {
	Dir    string
	Meta   Meta
	Locked bool
}

// List returns the incoming runs, oldest first. A folder without a readable
// run.json is listed with only its RunID (the folder's name) set.
func List(dataDir string) ([]Pending, error) {
	root := filepath.Join(dataDir, Dir)
	ents, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Pending
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		p := Pending{Dir: filepath.Join(root, e.Name()), Meta: Meta{RunID: e.Name()}}
		if b, err := os.ReadFile(filepath.Join(p.Dir, metaName)); err == nil {
			var m Meta
			if json.Unmarshal(b, &m) == nil {
				m.RunID = e.Name()
				p.Meta = m
			}
		}
		release, err := logfile.TryLock(p.Dir)
		var locked *logfile.LockedError
		switch {
		case errors.As(err, &locked):
			p.Locked = true
		case err == nil:
			release()
		}
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Meta.Started.Before(out[j].Meta.Started) })
	return out, nil
}

// Adopt reads an ended run's records and spans, in order, into line and span,
// then deletes its folder. A run still writing is *logfile.LockedError.
func Adopt(p Pending, line, span func([]byte)) error {
	release, err := logfile.TryLock(p.Dir)
	if err != nil {
		return err
	}
	err = read(p.Dir, line, span)
	release()
	if err != nil {
		return err
	}
	return os.RemoveAll(p.Dir)
}

func read(dir string, line, span func([]byte)) error {
	logs, err := logfile.HistoricalFiles(dir)
	if err != nil {
		return err
	}
	for _, path := range logs {
		if err := eachFile(path, func(f *os.File) error { return ingest.Stored(f, line) }); err != nil {
			return err
		}
	}
	spans, err := logfile.HistoricalSpanFiles(dir)
	if err != nil {
		return err
	}
	for _, path := range spans {
		err := eachFile(path, func(f *os.File) error {
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 64*1024), 64<<20)
			for sc.Scan() {
				if len(sc.Bytes()) > 0 {
					span(sc.Bytes())
				}
			}
			return sc.Err()
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func eachFile(path string, f func(*os.File) error) error {
	fh, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer fh.Close()
	return f(fh)
}
