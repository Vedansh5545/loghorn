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

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/ghjob"
	"github.com/jimbeveridge/loghorn/internal/ingest"
	"github.com/jimbeveridge/loghorn/internal/logfile"
	"github.com/jimbeveridge/loghorn/internal/store"
)

func usage() {
	fmt.Fprint(flag.CommandLine.Output(), `loghorn — wrap a command, record its logs, surface the failures.

Usage:
  loghorn [flags] -- <command> [args...]   run the command and record its output
  <command> | loghorn [flags]              record a pipe
  loghorn -web                             browse the stored logs
  loghorn -web|-filter < file              replay a file
  loghorn -web|-filter -historical         replay the stored logs
  loghorn -web|-filter -gh-job <id|url>    replay a GitHub Actions job
  loghorn -web|-filter -gh-run <id|url>    replay one job of a GitHub Actions run

With a command after --, loghorn is a transparent wrapper: the command's
stdout and stderr reach the terminal unchanged (through a pty when stdout is a
terminal, so colours and prompts keep working), Ctrl-C reaches the command
directly, SIGTERM and SIGHUP are forwarded to it (SIGTERM is followed by
SIGKILL after -shutdown-grace), and loghorn exits with the command's status:
127 when it can't be found. Piped input is copied to stdout the same way.

Every record is also appended to .loghorn/loghorn.log in the directory loghorn
starts in, with escape sequences removed, so each project gets its own logs and
its own lock. Each finished day is kept as loghorn-YYYY-MM-DD.log, and days
before the last three are deleted. loghorn never creates or writes .loghorn
inside its own source tree.

-filter prints only the important records (errors and worse) to stdout instead
of everything. It works with every input, and with -web.

Replays read a finished log and never write .loghorn, so they need -web or
-filter to show anything: a file on stdin (loghorn < file); -historical, the
stored days oldest first, then today; -gh-job, one GitHub Actions job by job
ID or URL; and -gh-run, a run by run ID or URL, which shows its first failed
job (or its first job) and lists the others on stderr. The GitHub flags fetch
through the gh CLI, so gh must be installed and logged in; with a bare ID the
repository is the one the current directory's git remote points at. Replay
flags refuse a command or piped stdin, so from cron or CI pass </dev/null.

-otel (with a command) also starts an OTLP/HTTP receiver on 127.0.0.1 and
points the command's OpenTelemetry SDK at it through OTEL_* environment
variables; the log records it receives are stored with the command's output,
and spans in .loghorn/loghorn.traces.jsonl (kept and rotated like loghorn.log).

-web serves a query view on 127.0.0.1 (loopback only; -web-port picks the
port, default a free one) and prints its URL to stderr. It lists the streams
of structured records, with typed filters, a time range, search and sort,
saved schemas and favorite queries (under .config/loghorn in the repo). With
a command it serves until the command exits, with a pipe until end of input;
with a replay, or alone (the stored logs), until Ctrl-C.

Settings come from .config/loghorn/config.toml, found by walking up from the
directory loghorn starts in; the nearest one wins outright, and if there is
none, $XDG_CONFIG_HOME/loghorn/config.toml or ~/.config/loghorn/config.toml
applies. It selects how input is framed (format) and how LogEntry fields and
severities are read (field-naming, severity). Run with -config to see which
file is in effect.

Examples:
  loghorn -- npm run dev
  loghorn -filter -- go test ./...
  kubectl logs -f pod | loghorn -web
  loghorn -web
  loghorn -filter -historical </dev/null
  loghorn -web -gh-run https://github.com/owner/repo/actions/runs/43954926722

Flags:
`)
	flag.PrintDefaults()
}

// opts holds every parsed flag value plus the arguments after them.
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
	dbDir        string
	args         []string
}

func parseFlags() opts {
	var o opts
	flag.BoolVar(&o.filter, "filter", false, "print only important records to stdout instead of everything")
	flag.BoolVar(&o.notify, "notify", false, "fire a desktop notification when fresh errors occur")
	flag.DurationVar(&o.notifyMaxAge, "notify-max-age", time.Second, "skip notifications for errors older than this (by their log timestamp)")
	flag.DurationVar(&o.notifyReset, "notify-reset", 15*time.Second, "end an error burst after this quiet gap, so the next error notifies again")
	flag.BoolVar(&o.notifySound, "notify-sound", true, "play the system alert sound with notifications (with -notify)")
	flag.DurationVar(&o.grace, "shutdown-grace", 5*time.Second, "after forwarding SIGTERM to the command, SIGKILL it this much later")
	flag.BoolVar(&o.exclusive, "exclusive", false, "exit if another loghorn is already writing the log file, instead of running without one")
	flag.BoolVar(&o.historical, "historical", false, "replay the stored log files (oldest day first, then today); needs -web or -filter")
	flag.StringVar(&o.ghRun, "gh-run", "", "replay the first failed job of a GitHub Actions run, by run ID or run URL (needs gh, and -web or -filter)")
	flag.StringVar(&o.ghJob, "gh-job", "", "replay a GitHub Actions job's log, by job ID or job URL (needs gh, and -web or -filter)")
	flag.BoolVar(&o.config, "config", false, "print the config file in effect and the settings it resolves to, then exit")
	flag.BoolVar(&o.otel, "otel", false, "receive the command's OTLP/HTTP logs and traces on 127.0.0.1 and record them (needs -- <command>)")
	flag.BoolVar(&o.web, "web", false, "serve the web query view on 127.0.0.1 and print its URL to stderr")
	flag.IntVar(&o.webPort, "web-port", 0, "port for -web (0 picks a free one)")
	flag.StringVar(&o.dbDir, "db-dir", "", "directory for the DuckDB files and their Parquet archive (default: the .loghorn directory)")
	flag.Usage = usage
	flag.Parse()
	o.args = flag.Args()
	return o
}

// main validates everything that can fail with os.Exit before runCommand
// launches a child; after that it only exits with the child's status.
func main() {
	if err := removedFlagError(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "loghorn:", err)
		os.Exit(2)
	}
	o := parseFlags()

	if n := countSet(o.historical, o.ghJob != "", o.ghRun != ""); n > 1 {
		fmt.Fprintln(os.Stderr, "loghorn: use only one of -historical, -gh-job and -gh-run")
		os.Exit(2)
	}
	m := mode{kind: detectInput(o.args, os.Stdin, o.historical, o.ghJob, o.ghRun), web: o.web, filter: o.filter, otel: o.otel}
	// -config reads nothing, so it is exempt from the input check.
	if !o.config {
		if err := m.check(); err != nil {
			fmt.Fprintln(os.Stderr, "loghorn:", err)
			os.Exit(2)
		}
	}

	// One os.Getwd serves both the config file and the web view's start
	// directory. logDir makes its own call for the log file's directory; the
	// two agree only because nothing in this program calls os.Chdir. A cwd
	// that can't be resolved here (deleted out from under the process) is not
	// fatal: config discovery has nowhere to walk from, so the built-in
	// defaults apply. A run that needs the log file still fails in logDir.
	// A bad config FILE is a usage error, reported before anything is
	// launched or written.
	cwd, cwdErr := os.Getwd()
	cfg, cfgPath := config.Default(), ""
	if cwdErr != nil {
		fmt.Fprintln(os.Stderr, "loghorn: can't resolve the working directory, using default settings:", cwdErr)
	} else {
		var err error
		cfg, cfgPath, err = config.Load(cwd)
		if err != nil {
			fmt.Fprintln(os.Stderr, "loghorn:", err)
			os.Exit(2)
		}
	}
	if o.config {
		if cfgPath == "" {
			fmt.Println("no config file found; using defaults")
		} else {
			fmt.Println(cfgPath)
		}
		fmt.Print(cfg.String())
		return
	}

	switch m.kind {
	case kindCommand:
		os.Exit(runCommand(o, cfg, cwd))
	case kindPipe:
		os.Exit(runPipe(o, cfg, cwd))
	default:
		os.Exit(runReplay(m, o, cfg, cwd))
	}
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

// openLogFile opens the always-on log file in .loghorn under the directory
// loghorn was started from. A lock held by another loghorn is returned rather
// than fatal, so loghorn can run on without a file — unless exclusive, when it
// exits. Any other failure exits: the file is always on, so an unwritable
// directory is a setup error.
func openLogFile(exclusive bool) (*logfile.Writer, *logfile.LockedError) {
	dir, err := logDir()
	if err != nil {
		// "log file:" marks this as the side log's problem, not the thing being
		// launched — unlike -historical's setup errors below, which print the
		// plain "loghorn: <err>" because -historical has no side log to blame.
		fmt.Fprintln(os.Stderr, "loghorn: log file:", err)
		os.Exit(1)
	}
	w, err := logfile.Open(dir, time.Now, time.Local)
	var inTree *logfile.SourceTreeError
	if errors.As(err, &inTree) {
		fmt.Fprintln(os.Stderr, "loghorn:", err) // not a log-file fault: the wrong directory
		os.Exit(1)
	}
	var locked *logfile.LockedError
	if errors.As(err, &locked) {
		if !exclusive {
			return nil, locked
		}
		// Print the LockedError as-is: its own Error() is already the exact,
		// unprefixed "another loghorn (pid N) is writing logs in <dir>" message.
		fmt.Fprintln(os.Stderr, "loghorn:", err)
		os.Exit(1)
	}
	if err != nil {
		// "log file:" marks this as the side log's problem, not the thing being
		// launched — unlike -historical's setup errors below, which print the
		// plain "loghorn: <err>" because -historical has no side log to blame.
		fmt.Fprintln(os.Stderr, "loghorn: log file:", err)
		os.Exit(1)
	}
	return w, nil
}

// countSet counts the true values.
func countSet(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
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

// recordSink returns the function that copies every record to the log file.
// A multi-line JSON record is compacted first: the file holds one record per
// line, and -historical replay depends on that. The writer returns only its
// first failure and ignores records after it, so onFail runs at most once.
// Without a log file the sink does nothing.
func recordSink(w *logfile.Writer, onFail func(error)) func(rec []byte) {
	if w == nil {
		return func([]byte) {}
	}
	return func(rec []byte) {
		if err := w.Write(ingest.CompactRecord(rec)); err != nil {
			onFail(err)
		}
	}
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
