package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/ghjob"
	"github.com/jimbeveridge/loghorn/internal/ingest"
	"github.com/jimbeveridge/loghorn/internal/recorder"
	"github.com/jimbeveridge/loghorn/internal/store"
	"github.com/jimbeveridge/loghorn/internal/wrap"
)

const openUsage = `Usage: loghorn open [flags] <file | - | GitHub Actions URL>
       loghorn open [flags] -gh-job ID
       loghorn open [flags] -gh-run ID

Open a finished log in a temporary web view (its URL goes to stderr; Ctrl-C
ends it), or with -filter print its important records. Nothing is recorded.
A GitHub Actions run or job is fetched with the gh CLI; a run shows its first
failed job (or its first job) and lists the others.
`

// openSrc is the one log open reads.
type openSrc struct {
	kind  string // "file", "stdin", "gh-job" or "gh-run"
	value string // the path, URL or ID
}

// openSource decides open's source from its positional argument and its
// GitHub flags. A GitHub URL with /job/ in its path names a job, any other
// one a run.
func openSource(pos, ghJob, ghRun string) (openSrc, error) {
	var srcs []openSrc
	switch {
	case pos == "":
	case pos == "-":
		srcs = append(srcs, openSrc{"stdin", ""})
	case strings.HasPrefix(pos, "https://github.com/") || strings.HasPrefix(pos, "http://github.com/"):
		if strings.Contains(pos, "/job/") {
			srcs = append(srcs, openSrc{"gh-job", pos})
		} else {
			srcs = append(srcs, openSrc{"gh-run", pos})
		}
	default:
		srcs = append(srcs, openSrc{"file", pos})
	}
	if ghJob != "" {
		srcs = append(srcs, openSrc{"gh-job", ghJob})
	}
	if ghRun != "" {
		srcs = append(srcs, openSrc{"gh-run", ghRun})
	}
	if len(srcs) != 1 {
		return openSrc{}, errors.New("open needs one log: a file, -, a GitHub Actions URL, -gh-job ID or -gh-run ID")
	}
	return srcs[0], nil
}

func cmdOpen(args []string, env cliEnv) int {
	fs := newFlagSet("open", openUsage, env)
	filter := fs.Bool("filter", false, "print only the important records to stdout instead of serving the view")
	view := fs.Bool("view", false, "with -filter, also serve the view")
	webPort := fs.Int("web-port", 0, "port for the view (0 picks a free one)")
	ghJob := fs.String("gh-job", "", "a GitHub Actions job's log, by job ID or job URL (needs gh)")
	ghRun := fs.String("gh-run", "", "the first failed job of a GitHub Actions run, by run ID or run URL (needs gh)")
	pos, code, ok := parseCmd(fs, args, true)
	if !ok {
		return code
	}
	if len(pos) > 1 {
		return usageError(env, "open takes one log; got %q", pos)
	}
	first := ""
	if len(pos) == 1 {
		first = pos[0]
	}
	src, err := openSource(first, *ghJob, *ghRun)
	if err != nil {
		return usageError(env, "%v", err)
	}
	cfg, _, cwd, code, ok := loadRepoConfig(env)
	if !ok {
		return code
	}
	return openLog(src, *filter, !*filter || *view, *webPort, cfg, cwd, env)
}

// openLog reads one finished log and never records it. With filter its
// important records go to stdout; with view the temporary web view is served
// after reading, until SIGINT or SIGTERM.
func openLog(src openSrc, filter, view bool, webPort int, cfg config.Config, cwd string, env cliEnv) int {
	var data io.Reader
	frame := configuredFrame(cfg)
	mode := store.ModeHistorical
	switch src.kind {
	case "file":
		f, err := os.Open(src.value)
		if err != nil {
			fmt.Fprintln(env.stderr, "loghorn:", err)
			return 1
		}
		defer f.Close()
		data = f
		if storedLogName(src.value) {
			// A stored day can mix producers (JSON lines, then YAML
			// documents), so it is framed record by record.
			frame = ingest.Stored
		}
	case "stdin":
		data = env.stdin
	case "gh-job", "gh-run":
		data = bytes.NewReader(fetchGHLog(src))
		mode = store.ModeGHJob
	}

	// Listen before reading, so a port in use fails before the work is done.
	var ws *webServer
	if view {
		ws = startView(webPort, cwd, mode, cfg.Input)
		defer ws.close()
	}
	rec := recorder.New(recorder.Options{Store: ws.store(), Input: cfg.Input})
	var out io.Writer = io.Discard
	if filter {
		out = env.stdout
	}
	if err := wrap.Pipe(data, wrap.Options{Recorder: rec, Frame: frame, Filter: filter, Stdout: out}); err != nil {
		fmt.Fprintln(env.stderr, "loghorn:", err)
		return 1
	}
	if ws != nil {
		fmt.Fprintf(env.stderr, "loghorn: web view at %s\n", ws.url)
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		<-ctx.Done()
		stop()
	}
	return 0
}

// storedLogName reports whether path is named like one of loghorn's own
// stored days (loghorn.log, loghorn-2026-11-20.log).
func storedLogName(path string) bool {
	b := filepath.Base(path)
	return strings.HasPrefix(b, "loghorn") && strings.HasSuffix(b, ".log")
}

// fetchGHLog fetches the whole log of the job src names (a run's first failed
// job, for a run), so a missing gh, a failed login or a wrong ID is reported
// before anything is served. Every failure exits.
func fetchGHLog(src openSrc) []byte {
	var ref ghjob.Ref
	if src.kind == "gh-job" {
		var err error
		if ref, err = ghjob.Parse(src.value); err != nil {
			fmt.Fprintln(os.Stderr, "loghorn:", err)
			os.Exit(2)
		}
	} else {
		run, err := ghjob.ParseRun(src.value)
		if err != nil {
			fmt.Fprintln(os.Stderr, "loghorn:", err)
			os.Exit(2)
		}
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

// describeJobs tells which of a run's jobs -gh-run is showing, and lists the
// others so another can be named with -gh-job.
func describeJobs(w io.Writer, run string, jobs []ghjob.Job, chosen int) {
	which := "the first one"
	if jobs[chosen].Failed() {
		which = "the first failed one"
	}
	fmt.Fprintf(w, "loghorn: run %s has %d jobs; showing %s (pick another with -gh-job <id>):\n", run, len(jobs), which)
	items := jobItems(jobs)
	for i, j := range jobs {
		mark := "  "
		if i == chosen {
			mark = "* "
		}
		fmt.Fprintf(w, "  %s%d  %s\n", mark, j.ID, items[i])
	}
}
