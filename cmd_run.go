package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/datadir"
	"github.com/jimbeveridge/loghorn/internal/incoming"
	"github.com/jimbeveridge/loghorn/internal/ingest"
	"github.com/jimbeveridge/loghorn/internal/otlp"
	"github.com/jimbeveridge/loghorn/internal/recorder"
	"github.com/jimbeveridge/loghorn/internal/runner"
	"github.com/jimbeveridge/loghorn/internal/shipper"
	"github.com/jimbeveridge/loghorn/internal/sock"
	"github.com/jimbeveridge/loghorn/internal/wrap"
)

const runUsage = `Usage: loghorn run [flags] [--] <command> [args...]
       <command> | loghorn run [flags]

Run a command, passing its stdout and stderr through unchanged and exiting
with its status, and record its output with loghorn serve; or record piped
input, copying it to stdout. The command's OpenTelemetry SDK is pointed at
serve too. Flags go before the command: everything from the command on is the
command's.
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
	if sess, err := sock.Probe(path); err == nil {
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
	if len(pos) > 0 {
		return runChild(o, cfg, rec, target, env)
	}
	err := wrap.Pipe(env.stdin, wrap.Options{
		Recorder:  rec,
		Frame:     configuredFrame(cfg),
		StripANSI: true,
		Filter:    o.filter,
		Stdout:    env.stdout,
		Stderr:    env.stderr,
	})
	target.finish()
	if err != nil {
		fmt.Fprintln(env.stderr, "loghorn: input error:", err)
		return 1
	}
	return 0
}

// runChild runs the command with the target's environment and records its
// output, returning its exit status (127 when it can't be found).
func runChild(o opts, cfg config.Config, rec *recorder.Recorder, target runTarget, env cliEnv) int {
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
