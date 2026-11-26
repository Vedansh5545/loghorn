// Command loghorn captures, stores and queries the logs and traces of the apps
// on a developer's machine. Its commands (dispatch.go) are:
//
//	loghorn serve           the store: OTLP receiver, data folder, web explorer
//	loghorn run -- <cmd>    run a command transparently and send its output to serve
//	loghorn open <log>      show a finished log (a file, stdin, GitHub Actions)
//	loghorn query <url>     print a web view's data
//	loghorn status | ui     serve's state and OTEL environment; open the explorer
//
// serve (internal/serve) is the one long-running process; every other command
// finds it through its unix socket (internal/sock) and exits.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/jimbeveridge/loghorn/internal/ghjob"
)

func main() { os.Exit(dispatch(os.Args[1:], stdEnv())) }

// opts carries the flag values of run and query to the code that reads them.
type opts struct {
	filter       bool
	notify       bool
	notifyMaxAge time.Duration
	notifyReset  time.Duration
	notifySound  bool
	grace        time.Duration

	// query (cmd_query.go).
	url      string
	out      string
	format   string
	limit    int
	limitSet bool // -limit was given
	toSQL    bool
	sql      string // -sql: SQL text, @file or @-

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
