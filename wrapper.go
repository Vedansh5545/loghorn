package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/jimbeveridge/loghorn/internal/alert"
	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/entry"
	"github.com/jimbeveridge/loghorn/internal/ingest"
	"github.com/jimbeveridge/loghorn/internal/logfile"
	"github.com/jimbeveridge/loghorn/internal/recorder"
	"github.com/jimbeveridge/loghorn/internal/runner"
	"github.com/jimbeveridge/loghorn/internal/store"
	"github.com/jimbeveridge/loghorn/internal/web"
	"github.com/jimbeveridge/loghorn/internal/wrap"
)

// runCommand is the `-- cmd` path. Everything that can exit — the log file,
// the -web listener — is set up before the child starts; from then on
// failures are reported and the child's status is returned.
func runCommand(o opts, cfg config.Config, cwd string) int {
	logs := openRecordingLog(o)
	ws := startLiveWeb(o, cfg, cwd)
	rec := liveRecorder(o, cfg, logs, ws.store())

	sigs, stop := wrap.NotifySignals()
	usePTY := term.IsTerminal(int(os.Stdout.Fd()))
	child, err := runner.StartChild(o.args, runner.Options{PTY: usePTY})
	if err != nil {
		stop()
		ws.close()
		closeLog(logs)
		fmt.Fprintln(os.Stderr, "loghorn:", err)
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
	ws.close()
	child.Close()
	closeLog(logs)
	return status
}

// runPipe records piped stdin, copying it to stdout (or with -filter only the
// important records). -web stops at end of input.
func runPipe(o opts, cfg config.Config, cwd string) int {
	logs := openRecordingLog(o)
	ws := startLiveWeb(o, cfg, cwd)
	rec := liveRecorder(o, cfg, logs, ws.store())
	err := wrap.Pipe(os.Stdin, wrap.Options{
		Recorder:  rec,
		Frame:     configuredFrame(cfg),
		StripANSI: true,
		Filter:    o.filter,
		Stdout:    os.Stdout,
		Stderr:    os.Stderr,
	})
	ws.close()
	closeLog(logs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loghorn: input error:", err)
		return 1
	}
	return 0
}

// openRecordingLog opens the project log file for live input. A lock held by
// another loghorn is reported and the run goes on without a file.
func openRecordingLog(o opts) *logfile.Writer {
	logs, locked := openLogFile(o.exclusive)
	if locked != nil {
		fmt.Fprintf(os.Stderr, "loghorn: %v; running without a log file\n", locked)
	}
	return logs
}

func closeLog(w *logfile.Writer) {
	if w != nil {
		w.Close()
	}
}

// configuredFrame frames input with the config file's format.
func configuredFrame(cfg config.Config) func(io.Reader, func([]byte)) error {
	return func(r io.Reader, emit func([]byte)) error {
		return ingest.Records(r, cfg.Input.Format, emit)
	}
}

// liveRecorder records live input: to the log file, the web store if any, and
// desktop notifications with -notify. A log file failure is reported once and
// the run goes on.
func liveRecorder(o opts, cfg config.Config, logs *logfile.Writer, st *store.Store) *recorder.Recorder {
	var once sync.Once
	return recorder.New(recorder.Options{
		Sink: recordSink(logs, func(err error) {
			once.Do(func() { fmt.Fprintln(os.Stderr, "loghorn: log file stopped:", err) })
		}),
		Store: st,
		Input: cfg.Input,
		Live:  true,
		Alert: notifier(o),
	})
}

// notifier turns -notify into the recorder's Alert, or nil without it. The
// recorder calls it under its lock, so the coalescer needs none of its own.
func notifier(o opts) func(entry.Entry) {
	if !o.notify {
		return nil
	}
	c := alert.NewCoalescer(o.notifyMaxAge, o.notifyReset, nil)
	n := alert.BeeepNotifier{Sound: o.notifySound}
	return func(e entry.Entry) {
		if fire, title, body := c.Observe(e); fire {
			_ = n.Notify(title, body)
		}
	}
}

// webServer is a running -web view; the zero value (nil) is "no -web".
type webServer struct {
	st  *store.Store
	srv *http.Server
	url string
}

func (w *webServer) store() *store.Store {
	if w == nil {
		return nil
	}
	return w.st
}

func (w *webServer) close() {
	if w != nil {
		w.srv.Close()
	}
}

// startLiveWeb starts -web for live input, with the stored days preloaded,
// and prints its URL. Without -web it returns nil.
func startLiveWeb(o opts, cfg config.Config, cwd string) *webServer {
	if !o.web {
		return nil
	}
	ws := startWeb(o, cfg, cwd, store.ModeLive, time.Now())
	fmt.Fprintf(os.Stderr, "loghorn: web view at %s\n", ws.url)
	return ws
}

// startWeb listens and serves a store of the given mode; it does not print
// the URL. A live store is preloaded with the stored days first. A listen
// failure exits, so it must run before any child is launched.
func startWeb(o opts, cfg config.Config, cwd string, mode store.Mode, runStart time.Time) *webServer {
	st := store.New(mode, runStart)
	if mode == store.ModeLive {
		if dir, err := logDir(); err == nil {
			if paths, err := logfile.HistoricalFiles(dir); err == nil {
				if err := store.Preload(st, paths, cfg.Input); err != nil {
					fmt.Fprintln(os.Stderr, "loghorn: -web: preload:", err)
				}
			}
		}
	}
	ln, err := web.Listen(o.webPort)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loghorn: -web:", err)
		os.Exit(1)
	}
	srv := &http.Server{
		Handler:           web.New(web.Options{Store: st, StartDir: cwd}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	return &webServer{st: st, srv: srv, url: "http://" + ln.Addr().String() + "/"}
}
