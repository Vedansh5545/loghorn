package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/datadir"
	"github.com/jimbeveridge/loghorn/internal/incoming"
	"github.com/jimbeveridge/loghorn/internal/ingest"
	"github.com/jimbeveridge/loghorn/internal/otlp"
	"github.com/jimbeveridge/loghorn/internal/recorder"
	"github.com/jimbeveridge/loghorn/internal/runner"
	"github.com/jimbeveridge/loghorn/internal/serve"
	"github.com/jimbeveridge/loghorn/internal/shipper"
	"github.com/jimbeveridge/loghorn/internal/sock"
	"github.com/jimbeveridge/loghorn/internal/svcconf"
	"github.com/jimbeveridge/loghorn/internal/wrap"
)

const runUsage = `Usage: loghorn run [flags] [--] <command> [args...]
       <command> | loghorn run [flags]

Run a command, passing its stdout and stderr through unchanged and exiting
with its status, and record its output with loghorn serve; or record piped
input, copying it to stdout. The command's OpenTelemetry SDK is pointed at
serve too. Flags go before the command: everything from the command on is the
command's.

With -serve and no serve running, run starts one inside itself for this run,
on the usual data folder, socket and ports, and keeps serving the web view
after the command exits, until Ctrl-C or SIGTERM (when stdin is a terminal;
without one it stops with the run). Its exit status is still the command's.
An AI coding agent's run ignores -serve unless LOGHORN_ALLOW_AGENT=1.
`

// runFlags registers run's local flags on fs into o.
func runFlags(fs *flag.FlagSet, o *opts) {
	fs.BoolVar(&o.filter, "filter", false, "print only important records to stdout instead of everything")
	fs.BoolVar(&o.notify, "notify", false, "fire a desktop notification when fresh errors occur")
	fs.DurationVar(&o.notifyMaxAge, "notify-max-age", time.Second, "skip notifications for errors older than this (by their log timestamp)")
	fs.DurationVar(&o.notifyReset, "notify-reset", 15*time.Second, "end an error burst after this quiet gap, so the next error notifies again")
	fs.BoolVar(&o.notifySound, "notify-sound", true, "play the system alert sound with notifications (with -notify)")
	fs.DurationVar(&o.grace, "shutdown-grace", 5*time.Second, "after forwarding SIGTERM to the command, SIGKILL it this much later")
}

// runTarget is where a run's records go: sink gets each framed line, childEnv
// is the command's environment (nil = loghorn's own), and finish runs once the
// input has ended.
type runTarget struct {
	sink     func([]byte)
	childEnv []string
	finish   func()
	// attach, when set, is called with the run's recorder before the command
	// starts: the incoming target stores the command's own OTLP through it,
	// which turns on the recorder's dedupe of its JSON stdout lines.
	attach func(*recorder.Recorder)
}

func cmdRun(args []string, env cliEnv) int {
	fs := newFlagSet("run", runUsage, env)
	var o opts
	runFlags(fs, &o)
	service := fs.String("service", "", "service name for the recorded output (default the command's OTEL_SERVICE_NAME, else <repo>-<command>)")
	oneShot := fs.Bool("serve", false, "with no serve running, start one inside run for this run and keep serving the web view until Ctrl-C")
	serveDownFlag := fs.String("serve-down", "", "when serve isn't running: record (to the data folder, for serve to adopt), skip (record nothing) or fail (don't run the command); default [run] serve-down, else record")
	pos, code, ok := parseCmd(fs, args, false)
	if !ok {
		return code
	}
	o.args = pos
	if len(pos) == 0 && !stdinHasInput(env.stdin) {
		return usageError(env, "run needs a command (loghorn run -- <command>) or piped input")
	}
	downMode, code, ok := serveDownMode(*serveDownFlag, env)
	if !ok {
		return code
	}
	cfg, _, cwd, code, ok := loadRepoConfig(env)
	if !ok {
		return code
	}
	runID := otlp.NewRunID(time.Now())
	svc := runService(*service, os.Environ(), cwd, pos)

	// Decide where records go before anything starts.
	path := sock.DefaultPath()
	var target runTarget
	var srv *serve.Server
	sess, err := sock.Probe(path)
	agent := ""
	if err != nil && *oneShot {
		agent = runningUnderAgent()
	}
	switch {
	case err == nil || !*oneShot:
	case agent != "":
		fmt.Fprintf(env.stderr, "loghorn: -serve ignored: started by an AI agent (%s is set), which would leave serve running; set %s=1 to allow it\n", agent, allowAgentVar)
	case errors.Is(err, sock.ErrNotRunning):
		var serr error
		if srv, serr = startOneShot(env.stderr); serr == nil {
			sess, err = srv.Session(), nil
		} else if again, perr := sock.Probe(path); perr == nil {
			// Another serve started meanwhile: use it.
			sess, err = again, nil
		} else {
			fmt.Fprintln(env.stderr, "loghorn: can't start serve: "+serr.Error())
		}
	default:
		fmt.Fprintln(env.stderr, "loghorn: -serve ignored: something is on the socket but isn't answering")
	}
	if err == nil {
		target = connectServe(sess, svc, runID, cfg.Input, env.stderr)
	} else {
		var code int
		if target, code, ok = serveDown(path, err, downMode, incoming.Meta{
			RunID: runID, Service: svc, Input: cfg.Input, Started: time.Now(),
		}, env.stderr); !ok {
			return code
		}
	}

	rec := recorder.New(recorder.Options{
		Sink: target.sink, Input: cfg.Input, Live: true, Alert: notifier(o),
		DedupeAfterOTLP: target.attach != nil,
	})
	if target.attach != nil {
		target.attach(rec)
	}
	// A one-shot serve listens for its stop signals for the whole run, so
	// none falls to the default action (which would skip srv.Close) in the
	// gap between the command's signal handling and the post-run wait.
	var sig *oneShotSignals
	if srv != nil {
		sig = newOneShotSignals()
	}
	var status int
	if len(pos) > 0 {
		status = runChild(o, cfg, rec, target, env, sig.childExited)
	} else {
		pipeDone := sig.stopDuringPipe(srv, target, env.stderr)
		err := wrap.Pipe(env.stdin, wrap.Options{
			Recorder:  rec,
			Frame:     configuredFrame(cfg),
			StripANSI: true,
			Filter:    o.filter,
			Stdout:    env.stdout,
			Stderr:    env.stderr,
		})
		pipeDone()
		target.finish()
		if err != nil {
			fmt.Fprintln(env.stderr, "loghorn: input error:", err)
			status = 1
		}
	}
	if srv != nil {
		sig.serveUntilStopped(srv, term.IsTerminal(int(env.stdin.Fd())), env.stderr)
	}
	return status
}

// startOneShot starts serve inside run, as 'loghorn serve' would with no
// flags: the user-level config's ports, the default data folder and socket.
// Its error is worded for the person (see startError).
func startOneShot(log io.Writer) (*serve.Server, error) {
	cfg, cfgPath, err := config.LoadUser()
	if err != nil {
		return nil, err
	}
	dataDir, err := datadir.Dir()
	if err != nil {
		return nil, err
	}
	base, err := svcconf.BaseFromEnv()
	if err != nil {
		return nil, err
	}
	web, otlpPort := cfg.Serve.Resolved()
	srv, err := serve.Start(serve.Options{
		DataDir: dataDir, Socket: sock.DefaultPath(), WebPort: web, OTLPPort: otlpPort,
		Config: cfg, ConfigBase: base, Version: version(), Log: log,
	})
	if err != nil {
		return nil, errors.New(startError(err, cfgPath))
	}
	return srv, nil
}

// oneShotSignals holds a one-shot serve's stop signals. term (SIGTERM,
// SIGHUP) means stop now, even mid-run: run forwards those to the command, and
// whoever sent them wants run gone too. intr (Ctrl-C) during the command is
// the command's, so it is discarded when the command exits; after that it
// ends serving. A nil *oneShotSignals does nothing.
type oneShotSignals struct {
	term, intr chan os.Signal
}

func newOneShotSignals() *oneShotSignals {
	s := &oneShotSignals{term: make(chan os.Signal, 1), intr: make(chan os.Signal, 1)}
	signal.Notify(s.term, syscall.SIGTERM, syscall.SIGHUP)
	signal.Notify(s.intr, os.Interrupt)
	return s
}

// childExited runs as soon as the command has exited, before run's own signal
// forwarding stops: a Ctrl-C queued until now was the command's.
func (s *oneShotSignals) childExited() {
	if s == nil {
		return
	}
	select {
	case <-s.intr:
	default:
	}
}

// stopDuringPipe ends run when a stop signal arrives while piped input is
// still being read: it flushes what was read, closes serve, and exits with the
// shell's status for the signal. The returned func stops the watch.
func (s *oneShotSignals) stopDuringPipe(srv *serve.Server, target runTarget, stderr io.Writer) func() {
	if s == nil {
		return func() {}
	}
	done := make(chan struct{})
	quit := make(chan struct{})
	go func() {
		defer close(quit)
		var got os.Signal
		select {
		case got = <-s.term:
		case got = <-s.intr:
		case <-done:
			return
		}
		target.finish()
		s.closeServe(srv, stderr)
		os.Exit(128 + signalNumber(got))
	}()
	return func() {
		close(done)
		<-quit
	}
}

// serveUntilStopped keeps a one-shot serve up after its run, so the run can
// be browsed, until Ctrl-C, SIGTERM or SIGHUP, then closes it. A TERM or HUP
// that arrived during the run closes it at once, and so does having no
// terminal on stdin: nobody there could press Ctrl-C, and a script or CI job
// would wait forever.
func (s *oneShotSignals) serveUntilStopped(srv *serve.Server, interactive bool, stderr io.Writer) {
	select {
	case <-s.term:
		s.closeServe(srv, stderr)
		return
	default:
	}
	if !interactive {
		fmt.Fprintln(stderr, "loghorn: no terminal to stop serve from, so it stops with the run; the run is stored. Start 'loghorn serve' to browse it.")
		s.closeServe(srv, stderr)
		return
	}
	fmt.Fprintf(stderr, "loghorn: the run is done; serving %s until Ctrl-C\n", srv.Session().WebURL())
	select {
	case <-s.term:
	case <-s.intr:
	}
	s.closeServe(srv, stderr)
}

// closeServe stops listening for signals, so a second Ctrl-C during a slow
// close kills run outright, then closes serve.
func (s *oneShotSignals) closeServe(srv *serve.Server, stderr io.Writer) {
	signal.Stop(s.term)
	signal.Stop(s.intr)
	if err := srv.Close(); err != nil {
		fmt.Fprintln(stderr, "loghorn: closing serve:", err)
	}
}

func signalNumber(sig os.Signal) int {
	if n, ok := sig.(syscall.Signal); ok {
		return int(n)
	}
	return 2
}

// runChild runs the command with the target's environment and records its
// output, returning its exit status (127 when it can't be found).
func runChild(o opts, cfg config.Config, rec *recorder.Recorder, target runTarget, env cliEnv, exited func()) int {
	sigs, stop := wrap.NotifySignals()
	usePTY := term.IsTerminal(int(os.Stdout.Fd()))
	child, err := runner.StartChild(o.args, runner.Options{PTY: usePTY, Env: target.childEnv})
	if err != nil {
		stop()
		target.finish()
		fmt.Fprintln(env.stderr, "loghorn:", err)
		if errors.Is(err, exec.ErrNotFound) {
			return 127 // the shell's status for a command that can't be found
		}
		return 1
	}
	var terminal *os.File
	if usePTY {
		terminal = os.Stdout
	}
	status := wrap.Run(child, wrap.Options{
		Recorder:  rec,
		Frame:     configuredFrame(cfg),
		StripANSI: true,
		Filter:    o.filter,
		Stdout:    os.Stdout,
		Stderr:    os.Stderr,
		Grace:     o.grace,
		Signals:   sigs,
		Terminal:  terminal,
	})
	exited()
	stop()
	child.Close()
	target.finish()
	return status
}

// connectServe sends the run's records to serve and points the command's
// OpenTelemetry SDK at serve's OTLP port.
func connectServe(sess sock.Session, svc, runID string, in config.Input, stderr io.Writer) runTarget {
	sh := shipper.New(sess.OTLPEndpoint(), shipper.Resource{Service: svc, RunID: runID, Input: in}, func(err error) {
		fmt.Fprintf(stderr, "loghorn: serve stopped answering (%v); output still passes through\n", err)
	})
	fmt.Fprintf(stderr, "loghorn: recording to serve as %s (run %s); web view at %s\n", svc, runID, sess.WebURL())
	return runTarget{
		sink:     sh.Line,
		childEnv: otlp.ChildEnv(os.Environ(), sess.OTLPEndpoint(), runID),
		finish: func() {
			if n := sh.Close(3 * time.Second); n > 0 {
				fmt.Fprintf(stderr, "loghorn: %d lines were not recorded\n", n)
			}
		},
	}
}

// serveDownMode resolves what run does when serve isn't running: the
// -serve-down flag, else the user-level [run] serve-down, else record.
func serveDownMode(flagValue string, env cliEnv) (mode string, code int, ok bool) {
	if flagValue != "" {
		if !slices.Contains(config.ServeDownModes, flagValue) {
			return "", usageError(env, "-serve-down %q is not one of %s", flagValue, strings.Join(config.ServeDownModes, ", ")), false
		}
		return flagValue, 0, true
	}
	cfg, _, err := config.LoadUser()
	if err != nil {
		fmt.Fprintln(env.stderr, "loghorn:", err)
		return "", 2, false
	}
	return cfg.Run.Mode(), 0, true
}

// serveDown is the target when serve doesn't answer, by the serve-down mode:
// fail stops before the command starts (ok false, code 1); skip runs it
// without recording; record writes the run to the data folder's incoming/
// for serve to adopt, and degrades to skip when that can't be set up.
func serveDown(path string, err error, mode string, m incoming.Meta, stderr io.Writer) (runTarget, int, bool) {
	why := notRunningMessage(path)
	if !errors.Is(err, sock.ErrNotRunning) {
		why = fmt.Sprintf("serve on %s: %v", path, err)
	}
	skip := runTarget{finish: func() {}}
	switch mode {
	case config.ServeDownFail:
		fmt.Fprintln(stderr, "loghorn: "+why)
		return runTarget{}, 1, false
	case config.ServeDownSkip:
		fmt.Fprintf(stderr, "loghorn: %s; running without recording\n", why)
		return skip, 0, true
	}
	dataDir, derr := datadir.Dir()
	if derr == nil {
		var t runTarget
		if t, derr = connectIncoming(dataDir, m, stderr); derr == nil {
			return t, 0, true
		}
	}
	fmt.Fprintf(stderr, "loghorn: %s; can't record this run (%v); running without recording\n", why, derr)
	return skip, 0, true
}

// connectIncoming records the run into its own folder under the data folder's
// incoming/, and receives the command's own OTLP on a private port, storing
// it there too.
func connectIncoming(dataDir string, m incoming.Meta, stderr io.Writer) (runTarget, error) {
	logs, spans, err := incoming.Create(dataDir, m)
	if err != nil {
		return runTarget{}, err
	}
	ln, err := otlp.Listen()
	if err != nil {
		spans.Close()
		logs.Close()
		return runTarget{}, err
	}
	dir := filepath.Join(dataDir, incoming.Dir, m.RunID)
	fmt.Fprintf(stderr, "loghorn: serve isn't running; recording this run to %s (apps sending OTLP on their own aren't recorded until serve starts)\n", dir)

	var logOnce, spanOnce sync.Once
	sink := func(rec []byte) {
		if err := logs.Write(ingest.CompactRecord(rec)); err != nil {
			logOnce.Do(func() { fmt.Fprintln(stderr, "loghorn: log file stopped:", err) })
		}
	}
	var rcv *otlp.Receiver
	return runTarget{
		sink:     sink,
		childEnv: otlp.ChildEnv(os.Environ(), "http://"+ln.Addr().String(), m.RunID),
		attach: func(rec *recorder.Recorder) {
			rcv = otlp.NewReceiver(rec.OTLP, func(line []byte) {
				if err := spans.Write(line); err != nil {
					spanOnce.Do(func() { fmt.Fprintln(stderr, "loghorn: span file stopped:", err) })
				}
			}, nil)
			rcv.Serve(ln)
		},
		finish: func() {
			if rcv != nil {
				// Deliveries must finish before the files close.
				rcv.Drain(500*time.Millisecond, 3*time.Second)
			} else {
				ln.Close()
			}
			spans.Close()
			logs.Close()
		},
	}, nil
}

// notRunningMessage is every client's word for a serve that doesn't answer.
func notRunningMessage(path string) string {
	return fmt.Sprintf("loghorn serve isn't running (no answer on %s); start it with 'loghorn serve' or install it with 'loghorn service install'", path)
}
