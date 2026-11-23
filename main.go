// Command loghorn wraps a command, passing its output through unchanged while
// recording every record, and focuses attention on the important lines.
//
//	loghorn -- npm run dev
//
// loghorn starts the command in its own process group, copies its stdout and
// stderr to loghorn's own (through a pty when loghorn's stdout is a terminal,
// so colour and line editing keep working), forwards signals, and exits with
// the command's status. Piped input (`npm run dev | loghorn`) passes through
// the same way.
//
// Every record is also appended to .loghorn/loghorn.log in the directory loghorn
// is started from; see package logfile. -web serves a query view of the records;
// -filter prints only the important ones. Replays of a finished log (a file on
// stdin, -historical, -gh-job, -gh-run) need -web or -filter.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jimbeveridge/loghorn/internal/ghjob"
	"github.com/jimbeveridge/loghorn/internal/logfile"
	"github.com/jimbeveridge/loghorn/internal/store"
)

func main() { os.Exit(dispatch(os.Args[1:], stdEnv())) }

// opts carries the flag values of run, open and query to the code paths they
// share.
type opts struct {
	filter       bool
	notify       bool
	notifyMaxAge time.Duration
	notifyReset  time.Duration
	notifySound  bool
	grace        time.Duration
	exclusive    bool
	historical   bool
	ghRun        string
	ghJob        string
	config       bool
	web          bool
	otel         bool
	webPort      int

	// Query mode (query_cli.go).
	url       string
	out       string
	format    string
	limit     int
	limitSet  bool // -limit was given
	toSQL     bool
	instances bool

	args []string
}

// webStoreMode picks the web store's mode and run start. Only a live run has a
// run start (and preloads stored days); -historical, the GitHub flags and
// stdin redirected from a file (fileInput) all replay a finished log, so they
// get a historical-style store with a zero run start.
func webStoreMode(historical, ghMode, fileInput bool, now time.Time) (store.Mode, time.Time) {
	switch {
	case ghMode:
		return store.ModeGHJob, time.Time{}
	case historical || fileInput:
		return store.ModeHistorical, time.Time{}
	}
	return store.ModeLive, now
}

// bareIDHint follows a gh failure when no repository was named, since the
// usual cause is running outside the repository's checkout.
const bareIDHint = "loghorn: a bare ID is looked up in this directory's GitHub repository; pass the URL to name another"

// jobItems lines the jobs up as "name  outcome" with the names padded to one
// width, for describeJobs.
func jobItems(jobs []ghjob.Job) []string {
	w := 0
	for _, j := range jobs {
		w = max(w, len(j.Name))
	}
	items := make([]string, len(jobs))
	for i, j := range jobs {
		items[i] = fmt.Sprintf("%-*s  %s", w, j.Name, j.Outcome())
	}
	return items
}

// refuseOtherInput exits with a usage error if a replay flag (-historical,
// -gh-job, -gh-run) was given a command to launch or a stdin carrying its own stream. A
// Stat error is treated as a terminal, i.e. allowed, matching isRegularFile's
// convention of failing open rather than closed on an unidentifiable stdin.
func refuseOtherInput(what string) {
	if len(flag.Args()) > 0 {
		fmt.Fprintf(os.Stderr, "loghorn: %s; it can't be combined with a command\n", what)
		os.Exit(2)
	}
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice == 0 {
		fmt.Fprintf(os.Stderr, "loghorn: %s; it can't also read stdin\n", what)
		os.Exit(2)
	}
}

// logDir is .loghorn under the directory loghorn was started from: where the
// log file, its archives and its lock live, and where -historical reads from.
// Deriving it from the current directory rather than the executable's gives
// every project its own lock — one binary serving many projects otherwise
// meant every loghorn on the machine shared a single lock.
func logDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(cwd, ".loghorn"), nil
}

// openHistoricalFiles resolves -historical's stored files and opens every one
// of them before any is read, which narrows but does not close the window for
// a concurrent midnight rename by a recording loghorn: once a file is open, an
// open descriptor survives the rename, but a rename landing between listing
// and the open of loghorn.log can still skip the day it was just archived to.
// A file that vanished between listing and opening was pruned in that window
// and is simply skipped; any other failure here is fatal, like every other
// -historical setup problem.
func openHistoricalFiles() []*os.File {
	dir, err := logDir()
	if err != nil {
		// Unprefixed by "log file:", unlike openLogFile's failure above:
		// -historical isn't about the always-on log, it's about reading what's
		// already on disk.
		fmt.Fprintln(os.Stderr, "loghorn:", err)
		os.Exit(1)
	}
	paths, err := logfile.HistoricalFiles(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loghorn:", err)
		os.Exit(1)
	}
	if len(paths) == 0 {
		fmt.Fprintf(os.Stderr, "loghorn: no stored logs in %s\n", dir)
		os.Exit(1)
	}
	files := make([]*os.File, 0, len(paths))
	for _, p := range paths {
		f, err := os.Open(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "loghorn:", err)
			os.Exit(1)
		}
		files = append(files, f)
	}
	return files
}
