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
	"fmt"
	"os"
	"time"

	"github.com/jimbeveridge/loghorn/internal/ghjob"
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
