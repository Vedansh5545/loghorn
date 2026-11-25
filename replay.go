package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/ghjob"
	"github.com/jimbeveridge/loghorn/internal/ingest"
	"github.com/jimbeveridge/loghorn/internal/recorder"
	"github.com/jimbeveridge/loghorn/internal/store"
	"github.com/jimbeveridge/loghorn/internal/wrap"
)

// runReplay reads a finished log once — a file on stdin, the stored days, or
// a GitHub Actions job — and never opens the log file. With -filter the
// important records go to stdout; with -web the view is served after reading
// until SIGINT or SIGTERM. kindNone (only with -web) replays the stored days.
func runReplay(m mode, o opts, cfg config.Config, cwd string) int {
	kind := m.kind
	if kind == kindNone {
		kind = kindHistorical
	}

	var sources []io.Reader
	frame := configuredFrame(cfg)
	switch kind {
	case kindHistorical:
		refuseOtherInput("-historical reads the stored logs")
		files := openHistoricalFiles()
		defer func() {
			for _, f := range files {
				f.Close()
			}
		}()
		for _, f := range files {
			sources = append(sources, f)
		}
		// A stored day can mix producers (JSON lines, then YAML documents), so
		// it is framed record by record rather than by the configured format.
		frame = ingest.Stored
	case kindGHJob, kindGHRun:
		sources = []io.Reader{bytes.NewReader(fetchGHLog(o))}
	case kindFile:
		sources = []io.Reader{os.Stdin}
	}

	// Listen before reading, so a port in use fails before the work is done.
	var ws *webServer
	var st *store.Store
	fromDays := false // the store reads the stored days itself
	if m.web {
		wl := listenWeb(o, cwd) // before the store, which can load days from text
		gh := kind == kindGHJob || kind == kindGHRun
		mode, runStart := webStoreMode(kind == kindHistorical, gh, kind == kindFile, time.Now())
		if kind == kindHistorical {
			if dir, err := logDir(); err == nil {
				if st, err = store.Open(store.Options{Mode: mode, Input: cfg.Input, Dir: dir, DBDir: o.dbDir}); err == nil {
					fromDays = true
				} else {
					fmt.Fprintln(os.Stderr, "loghorn: DuckDB:", err)
				}
			}
		}
		if st == nil {
			st = store.NewWithInput(mode, runStart, cfg.Input)
		}
		ws = wl.serve(st)
		defer ws.close()
	}

	recStore := st
	if fromDays {
		recStore = nil
	}
	rec := recorder.New(recorder.Options{Store: recStore, Input: cfg.Input})
	var out io.Writer = io.Discard
	if m.filter {
		out = os.Stdout
	}
	// Each source is read on its own, in order: the stored days oldest first.
	for _, src := range sources {
		if err := wrap.Pipe(src, wrap.Options{Recorder: rec, Frame: frame, Filter: m.filter, Stdout: out}); err != nil {
			fmt.Fprintln(os.Stderr, "loghorn:", err)
			return 1
		}
	}

	if ws != nil && kind == kindHistorical && !fromDays {
		if dir, err := logDir(); err == nil {
			preloadSpans(st, dir, cfg.Input)
		}
	}

	if ws != nil {
		fmt.Fprintf(os.Stderr, "loghorn: web view at %s\n", ws.url)
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		<-ctx.Done()
		stop()
	}
	return 0
}

// fetchGHLog resolves -gh-job or -gh-run and fetches the job's whole log, so
// a missing gh, a failed login or a wrong ID is reported before anything is
// served. Every failure exits.
func fetchGHLog(o opts) []byte {
	var ref ghjob.Ref
	if o.ghJob != "" {
		var err error
		if ref, err = ghjob.Parse(o.ghJob); err != nil {
			fmt.Fprintln(os.Stderr, "loghorn:", err)
			os.Exit(2)
		}
		refuseOtherInput("-gh-job reads a GitHub Actions log")
	} else {
		run, err := ghjob.ParseRun(o.ghRun)
		if err != nil {
			fmt.Fprintln(os.Stderr, "loghorn:", err)
			os.Exit(2)
		}
		refuseOtherInput("-gh-run reads a GitHub Actions log")
		ref = pickRunJob(run)
	}
	job, logBytes, err := ghjob.Fetch(ref)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loghorn:", err)
		if ref.Repo == "" {
			fmt.Fprintln(os.Stderr, bareIDHint)
		}
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "loghorn:", job.Describe(ref.Job))
	return logBytes
}

// pickRunJob chooses the run's job to show: its only job, or else the first
// failed one (the first job when none failed), listing them all on stderr so
// another can be named with -gh-job.
func pickRunJob(run ghjob.RunRef) ghjob.Ref {
	jobs, err := ghjob.Jobs(run)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loghorn:", err)
		if run.Repo == "" {
			fmt.Fprintln(os.Stderr, bareIDHint)
		}
		os.Exit(1)
	}
	switch len(jobs) {
	case 0:
		fmt.Fprintf(os.Stderr, "loghorn: run %s has no jobs\n", run.Run)
		os.Exit(1)
	case 1:
		return run.JobRef(jobs[0].ID)
	}
	chosen := ghjob.FirstFailed(jobs)
	describeJobs(os.Stderr, run.Run, jobs, chosen)
	return run.JobRef(jobs[chosen].ID)
}
