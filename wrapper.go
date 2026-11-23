package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/jimbeveridge/loghorn/internal/alert"
	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/entry"
	"github.com/jimbeveridge/loghorn/internal/ingest"
	"github.com/jimbeveridge/loghorn/internal/logfile"
	"github.com/jimbeveridge/loghorn/internal/otlp"
	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/recorder"
	"github.com/jimbeveridge/loghorn/internal/runner"
	"github.com/jimbeveridge/loghorn/internal/store"
	"github.com/jimbeveridge/loghorn/internal/svcconf"
	"github.com/jimbeveridge/loghorn/internal/web"
	"github.com/jimbeveridge/loghorn/internal/wrap"
)

// runCommand is the `-- cmd` path. Everything that can exit — the log file,
// the -web listener — is set up before the child starts; from then on
// failures are reported and the child's status is returned.
func runCommand(o opts, cfg config.Config, cwd string) int {
	logs := openRecordingLog(o)
	ws := startLiveWeb(o, cfg, cwd)
	// The preload is done, so stored lines stay "default"; live records take
	// the child's service name.
	if svc := childService(os.Environ()); svc != "" {
		ws.store().SetFallbackService(svc)
	}
	rec := liveRecorder(o, cfg, logs, ws.store())

	// The receiver is bound before the child starts: a failure exits here,
	// with nothing launched.
	var rcv *otlp.Receiver
	var childEnv []string
	if o.otel {
		ln, err := otlp.Listen()
		if err != nil {
			ws.close()
			closeLog(logs)
			fmt.Fprintln(os.Stderr, "loghorn: -otel:", err)
			return 1
		}
		id := otlp.NewRunID(time.Now())
		endpoint := "http://" + ln.Addr().String()
		// The span writer lives under the log writer's lock, and closeLog
		// (after rcv.Drain) closes it. Without a log file, traces are
		// not written, but with -web they still reach the store.
		spans := openSpanLog(logs)
		// The store was preloaded inside startLiveWeb above, before this
		// receiver exists, so a span is preloaded or hooked, never both.
		rcv = otlp.NewReceiver(rec.OTLP, spanSink(spans, ws.store(), func(err error) {
			fmt.Fprintln(os.Stderr, "loghorn: span file stopped:", err)
		}), nil)
		rcv.Serve(ln)
		fmt.Fprintf(os.Stderr, "loghorn: -otel: receiving OTLP at %s (run %s)\n", endpoint, id)
		childEnv = otlp.ChildEnv(os.Environ(), endpoint, id)
	}

	sigs, stop := wrap.NotifySignals()
	usePTY := term.IsTerminal(int(os.Stdout.Fd()))
	child, err := runner.StartChild(o.args, runner.Options{PTY: usePTY, Env: childEnv})
	if err != nil {
		stop()
		if rcv != nil {
			rcv.Drain(0, 0)
		}
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
	if rcv != nil {
		// Deliveries must finish before the web server and log file close.
		rcv.Drain(500*time.Millisecond, 3*time.Second)
	}
	ws.close()
	child.Close()
	closeLog(logs)
	if rcv != nil && !rec.SawOTLP() {
		fmt.Fprintln(os.Stderr, "loghorn: -otel: no OTLP logs received; stdout was recorded instead")
	}
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

// openSpanLog opens the span file as a companion of the project log, under its
// lock. Without a log file there is none; a failure is reported and the run
// goes on without one.
func openSpanLog(logs *logfile.Writer) *logfile.Writer {
	if logs == nil {
		return nil
	}
	w, err := logs.Companion(logfile.SpansBase, logfile.SpansSuffix)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loghorn: span file:", err)
		return nil
	}
	return w
}

// spanSink returns the receiver's span callback: each line (already compact
// JSON) is appended to w if there is one, and added to st if there is one. A
// nil writer and nil store give a nil callback, so the receiver discards
// traces. onFail runs at most once, for the writer's first failure; the
// writer ignores spans after it. The store gets the span whatever happened
// to the file write.
func spanSink(w *logfile.Writer, st *store.Store, onFail func(error)) func(line []byte) {
	if w == nil && st == nil {
		return nil
	}
	var once sync.Once
	return func(line []byte) {
		if w != nil {
			if err := w.Write(line); err != nil {
				once.Do(func() { onFail(err) })
			}
		}
		if st != nil {
			if r, ok := record.NormalizeSpan(line); ok {
				st.Add(r, time.Now())
			}
		}
	}
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

		DedupeAfterOTLP: o.otel,
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

// preloadStored loads the stored log files, then the stored span files, into
// st. Errors are reported and do not stop startup.
func preloadStored(st *store.Store, dir string, in config.Input) {
	if paths, err := logfile.HistoricalFiles(dir); err == nil {
		if err := store.Preload(st, paths, in); err != nil {
			fmt.Fprintln(os.Stderr, "loghorn: -web: preload:", err)
		}
	}
	preloadSpans(st, dir, in)
}

// preloadSpans loads the stored span files in dir into st.
func preloadSpans(st *store.Store, dir string, in config.Input) {
	if paths, err := logfile.HistoricalSpanFiles(dir); err == nil {
		if err := store.Preload(st, paths, in); err != nil {
			fmt.Fprintln(os.Stderr, "loghorn: -web: preload:", err)
		}
	}
}

// startWeb listens and serves a store of the given mode; it does not print
// the URL. A live store is preloaded with the stored days first. A config
// base or listen failure exits, so it must run before any child is launched.
// It prints the one-time notice about a repo's retired .config/loghorn.
func startWeb(o opts, cfg config.Config, cwd string, mode store.Mode, runStart time.Time) *webServer {
	base, err := svcconf.BaseFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "loghorn: -web: config dir:", err)
		os.Exit(1)
	}
	if msg := repoConfigNotice(cwd, base); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
	}
	st := store.NewWithInput(mode, runStart, cfg.Input)
	if mode == store.ModeLive {
		if dir, err := logDir(); err == nil {
			preloadStored(st, dir, cfg.Input)
		}
	}
	ln, err := web.Listen(o.webPort)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loghorn: -web:", err)
		os.Exit(1)
	}
	srv := &http.Server{
		Handler:           web.New(web.Options{Store: st, ConfigBase: base}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	return &webServer{st: st, srv: srv, url: "http://" + ln.Addr().String() + "/"}
}

// childService returns the value of the last OTEL_SERVICE_NAME entry in env,
// or "" when there is none.
func childService(env []string) string {
	out := ""
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "OTEL_SERVICE_NAME="); ok {
			out = v
		}
	}
	return out
}
