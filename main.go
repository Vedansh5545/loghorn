// Command loghorn captures GCP Cloud Run logs and focuses attention on the
// important lines.
//
// Prefer launching the producer:
//
//	loghorn -- npm run dev
//
// loghorn then owns the process: it captures stdout and stderr together, keeps the
// keyboard to itself, and shuts the whole process group down on quit. Piping
// (`npm run dev | loghorn`) still works and is right for files and non-interactive
// producers, but an interactive one fights loghorn for /dev/tty — both processes
// read it, so keystrokes get split between them at random.
//
// Every record is also appended to .loghorn/loghorn.log in the directory loghorn
// is started from; see package logfile. Run with -historical to read those
// stored files back instead of live input.
//
// Run with --filter for a headless stdin->stdout filter.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/lucasb-eyer/go-colorful"
	"github.com/muesli/termenv"

	"github.com/jimbeveridge/loghorn/internal/adapter"
	"github.com/jimbeveridge/loghorn/internal/alert"
	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/engine"
	"github.com/jimbeveridge/loghorn/internal/entry"
	"github.com/jimbeveridge/loghorn/internal/ghjob"
	"github.com/jimbeveridge/loghorn/internal/headless"
	"github.com/jimbeveridge/loghorn/internal/ingest"
	"github.com/jimbeveridge/loghorn/internal/logfile"
	"github.com/jimbeveridge/loghorn/internal/runner"
	"github.com/jimbeveridge/loghorn/internal/tui"
)

func usage() {
	fmt.Fprint(flag.CommandLine.Output(), `loghorn — watch logs, surface the failures.

Usage:
  loghorn [flags] -- <command> [args...]   launch the command and capture it (preferred)
  <command> | loghorn [flags]              read a pipe

Launching is preferred for an interactive producer such as a dev server: loghorn
captures its stdout and stderr together, keeps the keyboard to itself, and stops
its whole process group on quit. In a pipeline the producer still holds the
terminal, so it competes with loghorn for keystrokes and outlives it.

Every record is also appended to .loghorn/loghorn.log in the directory loghorn
starts in, so each project gets its own logs and its own lock. Each finished day
is kept as loghorn-YYYY-MM-DD.log, and days before the last three are deleted.
Input read from a file (loghorn < file) is not recorded — it's already on disk.
loghorn never creates or writes .loghorn inside its own source tree, so
recording live input there fails; reading a file, -historical and the GitHub
flags still work. Read the stored files back with -historical: oldest day first, then today, read-only, stopping at the
end rather than following the live file. -historical refuses piped stdin or
stdin redirected from a file, so from cron or CI pass </dev/null.

Settings come from .config/loghorn/config.toml, found by walking up from the
directory loghorn starts in; the nearest one wins outright, and if there is
none, $XDG_CONFIG_HOME/loghorn/config.toml or ~/.config/loghorn/config.toml
applies. It selects how input is framed (format) and how LogEntry fields and
severities are read (field-naming, severity) — gcloud's snake_case and
numeric severities are accepted by default. Run with -config to see which
file is in effect.

-gh-job reads one finished GitHub Actions job's log instead of live input. Give
it the job ID (the number after /job/ in the job's URL) or the whole URL; the
run it belongs to is looked up. -gh-run takes a run ID or run URL instead and
asks at startup which of the run's jobs to show, starting on the first failed
one. Both fetch through the GitHub
CLI, so gh must be installed and logged in; with a bare ID the repository is
the one the current directory's git remote points at (as for gh run view).
Like -historical they are read-only: nothing is written to .loghorn.

Examples:
  loghorn -- npm run dev
  loghorn --scrollback 20000 -- go test ./...
  kubectl logs -f pod | loghorn
  loghorn -historical
  loghorn -gh-job 133607019104
  loghorn -gh-job https://github.com/owner/repo/actions/runs/43954926722/job/133607019104
  loghorn -gh-run https://github.com/owner/repo/actions/runs/43954926722

Flags:
`)
	flag.PrintDefaults()
}

// childControl adapts the runner to what the TUI needs, binding in the
// configured shutdown grace period.
type childControl struct {
	r     *runner.Runner
	grace time.Duration
}

func (c childControl) Send(p []byte) error { return c.r.Send(p) }
func (c childControl) Terminate()          { c.r.Terminate(c.grace) }
func (c childControl) Name() string        { return c.r.Name() }

func main() {
	filterMode := flag.Bool("filter", false, "headless: print only important lines to stdout")
	capacity := flag.Int("scrollback", 5000, "max entries kept in memory")
	notify := flag.Bool("notify", false, "fire a desktop notification when fresh errors occur")
	notifyMaxAge := flag.Duration("notify-max-age", time.Second, "skip notifications for errors older than this (by their log timestamp)")
	notifyReset := flag.Duration("notify-reset", 15*time.Second, "end an error burst after this quiet gap, so the next error notifies again")
	notifySound := flag.Bool("notify-sound", true, "play the system alert sound with notifications (with --notify)")
	grace := flag.Duration("shutdown-grace", 5*time.Second, "how long a launched command gets to exit on SIGTERM before SIGKILL")
	exclusive := flag.Bool("exclusive", false, "exit if another loghorn is already writing the log file, instead of running without one")
	historical := flag.Bool("historical", false, "show the stored log files (oldest day first, then today) instead of live input")
	ghRunFlag := flag.String("gh-run", "", "pick one job of a GitHub Actions run, by run ID or run URL, and show its log (needs gh)")
	ghJobFlag := flag.String("gh-job", "", "show a GitHub Actions job's log, by job ID or job URL, instead of live input (needs gh)")
	theme := flag.String("theme", "auto", "colours: auto (ask the terminal for its background), light, dark, or the background as #rrggbb")
	configFlag := flag.Bool("config", false, "print the config file in effect and the settings it resolves to, then exit")
	flag.Usage = usage
	flag.Parse()

	// scrollbackFor needs to know whether --scrollback was passed explicitly,
	// as opposed to just holding its default value, so Visit (which only
	// reaches flags actually set on the command line) rather than Lookup.
	explicitScrollback := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "scrollback" {
			explicitScrollback = true
		}
	})

	// A bad --theme is a usage error, reported before anything is launched.
	themeBackground, askTerminal, err := parseTheme(*theme)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loghorn:", err)
		os.Exit(2)
	}

	// One os.Getwd serves both the config file and the source-tree refusal
	// below. logDir (below) makes its own separate call for the log file's
	// directory; the two calls agree with each other only because nothing in
	// this program calls os.Chdir, not because there is one shared
	// resolution. A cwd that can't be resolved here (e.g. it was deleted out
	// from under the process) is not fatal for either of these two checks:
	// the source-tree check fails open by design, and config discovery has
	// nowhere to walk from, so both are skipped and the built-in defaults
	// apply rather than the whole run failing over it. That fail-open is
	// only partial: a normal run still needs the log file, and logDir's own
	// os.Getwd() exits 1 there on the same unresolvable cwd, so surviving it
	// is only possible for -config and for replaying a file over stdin,
	// which never open the log file at all.
	// A bad config FILE is still a usage error like a bad --theme, reported
	// before anything is launched or written.
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
	if *configFlag {
		if cfgPath == "" {
			fmt.Println("no config file found; using defaults")
		} else {
			fmt.Println(cfgPath)
		}
		fmt.Print(cfg.String())
		return
	}

	// -historical and -gh-job read a finished log, so they reject the two things
	// that would otherwise compete with that: a command to launch (there is
	// nothing to launch — it's a reader) and stdin carrying its own stream; see
	// refuseOtherInput. The two also exclude each other.
	if n := countSet(*historical, *ghJobFlag != "", *ghRunFlag != ""); n > 1 {
		fmt.Fprintln(os.Stderr, "loghorn: use only one of -historical, -gh-job and -gh-run")
		os.Exit(2)
	}
	// replay covers every reader of a finished log: none launches anything,
	// reads stdin or writes the log file.
	replay := *historical || *ghJobFlag != "" || *ghRunFlag != ""
	var historicalFiles []*os.File
	if *historical {
		refuseOtherInput("-historical reads the stored logs")
		historicalFiles = openHistoricalFiles()
		for _, f := range historicalFiles {
			defer f.Close()
		}
	}
	// The job's log is fetched whole before anything starts, so a missing gh,
	// a failed login or a wrong ID is reported here rather than inside the TUI.
	// ghMode is decided by the flags, never by ghLog's content: an empty log
	// (a skipped job) comes back nil, and testing ghLog would then fall through
	// to reading stdin — the terminal — as if it were a pipe.
	ghMode := *ghJobFlag != "" || *ghRunFlag != ""
	var ghLog []byte
	var ghAbout string
	if ghMode {
		var ref ghjob.Ref
		if *ghJobFlag != "" {
			var err error
			if ref, err = ghjob.Parse(*ghJobFlag); err != nil {
				fmt.Fprintln(os.Stderr, "loghorn:", err)
				os.Exit(2)
			}
			refuseOtherInput("-gh-job reads a GitHub Actions log")
		} else {
			run, err := ghjob.ParseRun(*ghRunFlag)
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
		ghLog, ghAbout = logBytes, job.Describe(ref.Job)
		fmt.Fprintln(os.Stderr, "loghorn:", ghAbout)
	}

	// The log file opens before --filter dispatch and before a child is launched,
	// so --exclusive never starts a producer only to abandon it. Replaying a file
	// (no command after `--`, and stdin is a regular file) is the exception: a
	// file on disk is already recorded, and once measured, feeding loghorn.log
	// back into loghorn through --filter appended every record it read to the
	// same file it was reading, so it never reached EOF (3 lines became 357,965
	// in one second). So a replay opens no log file at all: no lock, no write,
	// no bar marker, no stderr message. -historical is read-only in the same
	// spirit — it never opens the log file at all, so it works even while
	// another loghorn is recording, and --exclusive has no effect on it.
	var logs *logfile.Writer
	var locked *logfile.LockedError
	if !replay && !(len(flag.Args()) == 0 && isRegularFile(os.Stdin)) {
		logs, locked = openLogFile(*exclusive)
	}
	if logs != nil {
		defer logs.Close()
	}

	if *filterMode {
		if *historical {
			// Each file is run through the engine on its own rather than
			// concatenated, and framed by ingest.Stored rather than by the
			// configured format: one stored day can hold runs from different
			// producers, so it is framed line by line. No sink: -historical
			// never writes to the log file.
			for _, f := range historicalFiles {
				if err := headless.RunStored(f, os.Stdout, cfg.Input); err != nil {
					fmt.Fprintln(os.Stderr, "loghorn:", err)
					os.Exit(1)
				}
			}
			return
		}
		if ghMode {
			if err := headless.Run(bytes.NewReader(ghLog), os.Stdout, cfg.Input, recordSink(nil, nil)); err != nil {
				fmt.Fprintln(os.Stderr, "loghorn:", err)
				os.Exit(1)
			}
			return
		}
		if locked != nil {
			fmt.Fprintf(os.Stderr, "loghorn: %v; running without a log file\n", locked)
		}
		sink := recordSink(logs, func(err error) {
			fmt.Fprintln(os.Stderr, "loghorn: log file stopped:", err)
		})
		if err := headless.Run(os.Stdin, os.Stdout, cfg.Input, sink); err != nil {
			fmt.Fprintln(os.Stderr, "loghorn:", err)
			os.Exit(1)
		}
		return
	}

	// With a command after `--`, loghorn launches it and owns it: stdout and stderr
	// arrive together, the child is kept off the terminal so it can't steal
	// keystrokes, and quitting can shut its whole process group down. Without
	// one, read the pipe as before. -historical replaces this with its own
	// ordered list of opened files (the -historical guards above already rule
	// out a command after `--`, so argv is always empty in that case).
	var sources []io.Reader
	var child *runner.Runner
	if *historical {
		for _, f := range historicalFiles {
			sources = append(sources, f)
		}
	} else if ghMode {
		sources = []io.Reader{bytes.NewReader(ghLog)}
	} else {
		var source io.Reader = os.Stdin
		if argv := flag.Args(); len(argv) > 0 {
			r, err := runner.Start(argv)
			if err != nil {
				fmt.Fprintln(os.Stderr, "loghorn:", err)
				os.Exit(1)
			}
			child, source = r, r.Output()
			defer child.Close()
		}
		sources = []io.Reader{source}
	}

	ch := make(chan entry.Entry, 1024)

	var coalescer *alert.Coalescer
	var notifier alert.Notifier
	if *notify {
		coalescer = alert.NewCoalescer(*notifyMaxAge, *notifyReset, nil)
		notifier = alert.BeeepNotifier{Sound: *notifySound}
	}

	// Colours are resolved against the terminal's background before Bubble Tea
	// starts. The OSC 11 reply comes back on the terminal, and once the program is
	// reading /dev/tty it would arrive as keystrokes. --filter has returned by now;
	// it writes no colour, so it never asks.
	if askTerminal {
		themeBackground = terminalBackground()
	}
	tui.SetBackground(themeBackground)

	// loghorn consumes stdin for logs, so Bubble Tea can't use it for the UI:
	// keyboard AND resize (SIGWINCH) events must come from the controlling
	// terminal. Wire /dev/tty as the program input; without it Bubble Tea tries
	// to read key/resize input from the piped stdin, which delivers neither, so
	// the TUI never learns the terminal was resized. Fall back to the default
	// if there is no controlling tty (e.g. a headless environment).
	// Mouse capture starts on so clicking selects a line; 'm' toggles it off to
	// hand text selection back to the terminal for copying.
	opts := []tea.ProgramOption{tea.WithAltScreen(), tea.WithMouseCellMotion()}
	if tty, err := os.Open("/dev/tty"); err == nil {
		defer tty.Close()
		opts = append(opts, tea.WithInput(tty))
	}
	model := tui.NewModel(ch, scrollbackFor(replay, explicitScrollback, *capacity))
	model.SetDisplay(cfg.Display)
	if child != nil {
		model.SetChild(childControl{r: child, grace: *grace})
	}
	if locked != nil {
		model.SetLogFileOff(locked.Holder() + " has it")
	}
	if *historical {
		model.SetHistorical()
	}
	if ghMode {
		model.SetReplay("GH JOB", "reading GitHub Actions "+ghAbout)
	}
	p := tea.NewProgram(model, opts...)

	// logErr keeps a mid-run log file failure to print once the TUI has gone; the
	// bar marker only says that it happened. The writer reports at most one
	// failure, so the buffer of one never blocks.
	logErr := make(chan error, 1)
	sink := recordSink(logs, func(err error) {
		logErr <- err
		p.Send(tui.LogFileOff("write failed"))
	})

	// errCh carries a non-EOF read error from the producer goroutine to main.
	// It's buffered so the goroutine never blocks sending it, even if the TUI
	// has already quit (e.g. via 'q') and nobody is listening yet.
	// The goroutine starts only now because a log file failure reaches the TUI
	// through p.
	errCh := make(chan error, 1)
	parser := adapter.New(cfg.Input)
	go func() {
		// One goroutine loops over the sources in order rather than one per
		// source: live mode's list always has exactly one element, and
		// -historical's stored files must be read oldest-first, in sequence, not
		// interleaved. A stored file is framed by ingest.Stored, not by the
		// configured format: one stored day can mix producers (JSON lines, then
		// YAML documents), and framing chosen from its first line shreds the rest.
		frame := func(r io.Reader, emit func([]byte)) error {
			return ingest.Records(r, cfg.Input.Format, emit)
		}
		if *historical {
			frame = ingest.Stored
		}
		var err error
		for _, src := range sources {
			err = frame(src, func(line []byte) {
				sink(line)
				e := parser.ParseLine(line)
				e.Received = time.Now()
				e.Important = engine.IsImportant(e)
				if coalescer != nil && e.Important {
					if fire, title, body := coalescer.Observe(e); fire {
						_ = notifier.Notify(title, body)
					}
				}
				ch <- e
			})
			if err != nil {
				break
			}
		}
		close(ch)
		errCh <- err
	}()

	if child != nil {
		// Surface an unexpected exit in the TUI instead of tearing it down — a
		// crash is exactly when the scrollback is worth reading.
		go func() {
			<-child.Exited()
			p.Send(tui.ChildExited(child.ExitCode()))
		}()
	}
	_, runErr := p.Run()
	if runErr != nil {
		fmt.Fprintln(os.Stderr, "loghorn:", runErr)
		os.Exit(1)
	}
	// The producer may still be running if the TUI quit before stdin was
	// fully drained (e.g. the user pressed 'q'); don't block exit waiting
	// for it. Only report an error that was already available.
	select {
	case err := <-errCh:
		if err != nil {
			fmt.Fprintln(os.Stderr, "loghorn: input error:", err)
		}
	default:
	}
	select {
	case err := <-logErr:
		fmt.Fprintln(os.Stderr, "loghorn: log file stopped:", err)
	default:
	}
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

// pickRunJob lists the run's jobs and asks which one to show. A run with a
// single job needs no question. The picker starts on the first failed job,
// the one usually wanted. The picker reads keys from /dev/tty and
// draws on stderr, so it works under --filter too; without a terminal to ask
// on, it lists the jobs and exits, since -gh-job can name one directly.
func pickRunJob(run ghjob.RunRef) ghjob.Ref {
	jobs, err := ghjob.Jobs(run)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loghorn:", err)
		if run.Repo == "" {
			fmt.Fprintln(os.Stderr, bareIDHint)
		}
		os.Exit(1)
	}
	if len(jobs) == 1 {
		return run.JobRef(jobs[0].ID)
	}
	items := jobItems(jobs)
	tty, err := os.Open("/dev/tty")
	if err != nil {
		fmt.Fprintf(os.Stderr, "loghorn: run %s has %d jobs and there is no terminal to choose on; pick one with -gh-job:\n", run.Run, len(jobs))
		for i, j := range jobs {
			fmt.Fprintf(os.Stderr, "  %d  %s\n", j.ID, items[i])
		}
		os.Exit(2)
	}
	defer tty.Close()
	i, ok, err := tui.Pick(fmt.Sprintf("Run %s has %d jobs — which log?", run.Run, len(jobs)), items, ghjob.FirstFailed(jobs), tty, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loghorn:", err)
		os.Exit(1)
	}
	if !ok {
		os.Exit(0) // cancelled: nothing to show, nothing went wrong
	}
	return run.JobRef(jobs[i].ID)
}

// jobItems lines the jobs up as "name  outcome" with the names padded to one
// width, for the picker.
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

// isRegularFile reports whether f is a plain file rather than a pipe, socket,
// or character device such as an interactive terminal. A Stat error counts as
// not regular, so the caller falls back to treating it as something worth
// recording rather than silently skipping a log file it can't identify.
func isRegularFile(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode().IsRegular()
}

// parseTheme turns --theme into the background colours are resolved against.
// ask reports "auto": the terminal is asked, and bg is unused.
func parseTheme(s string) (bg colorful.Color, ask bool, err error) {
	switch s {
	case "auto":
		return colorful.Color{}, true, nil
	case "light":
		return colorful.Color{R: 1, G: 1, B: 1}, false, nil
	case "dark":
		return colorful.Color{}, false, nil
	}
	// Exactly #rrggbb: colorful.Hex also accepts the #rgb short form, which the
	// flag doesn't promise.
	if len(s) == 7 && s[0] == '#' {
		if c, err := colorful.Hex(s); err == nil {
			return c, false, nil
		}
	}
	return colorful.Color{}, false, errors.New("--theme must be auto, light, dark or #rrggbb")
}

// terminalBackground asks the terminal for its background colour (OSC 11). With
// no reply termenv falls back to COLORFGBG and then to black, whose palette keeps
// loghorn's usual text colours — except strings, which use a fixed green rather
// than a colour picked against the resolved background — so there is no separate
// failure to handle. termenv
// follows the query with a cursor-position request, which a terminal that ignores
// OSC 11 still answers at once; a terminal that answers neither stalls startup
// for termenv's 5 s timeout. So does an interactive piped producer reading the
// same terminal, which can swallow the reply before termenv sees it. --theme
// skips the query.
func terminalBackground() colorful.Color {
	return termenv.ConvertToRGB(termenv.NewOutput(os.Stdout).BackgroundColor())
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

// historicalScrollback is -historical's default ring capacity, used unless
// --scrollback was passed explicitly. Three or four stored days of typical
// dev-server output usually exceed the live default of 5,000 entries, and a
// small ring would silently evict the oldest of them; 100,000 comfortably
// covers that many days while still bounding memory rather than sizing the
// ring to whatever the archives happen to contain.
const historicalScrollback = 100000

// scrollbackFor decides the ring's capacity. An explicit --scrollback always
// wins; otherwise -historical gets historicalScrollback instead of n (the
// live default), since it is replaying whole stored days rather than
// watching one live stream.
func scrollbackFor(historical, explicit bool, n int) int {
	if historical && !explicit {
		return historicalScrollback
	}
	return n
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
