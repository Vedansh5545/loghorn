package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jimbeveridge/loghorn/internal/alert"
	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/entry"
	"github.com/jimbeveridge/loghorn/internal/ingest"
	"github.com/jimbeveridge/loghorn/internal/logfile"
	"github.com/jimbeveridge/loghorn/internal/store"
	"github.com/jimbeveridge/loghorn/internal/svcconf"
	"github.com/jimbeveridge/loghorn/internal/web"
)

// configuredFrame frames input with the config file's format.
func configuredFrame(cfg config.Config) func(io.Reader, func([]byte)) error {
	return func(r io.Reader, emit func([]byte)) error {
		return ingest.Records(r, cfg.Input.Format, emit)
	}
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

	sockSrv  *http.Server // the per-user unix socket; nil when it could not start
	sockPath string
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
		if w.sockSrv != nil {
			w.sockSrv.Close() // closes the listener, which unlinks the file
			_ = os.Remove(w.sockPath)
		}
	}
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
	st := store.New(mode, runStart)
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
	port := 0
	if a, ok := ln.Addr().(*net.TCPAddr); ok {
		port = a.Port
	}
	handler := web.New(web.Options{
		Store: st, ConfigBase: base,
		PID: os.Getpid(), Cwd: cwd, Port: port, Started: time.Now(),
	})
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	ws := &webServer{st: st, srv: srv, url: "http://" + ln.Addr().String() + "/"}
	ws.serveSocket(handler)
	return ws
}

// serveSocket also serves handler on this process's unix socket in the
// per-user socket dir, where the CLI finds running instances. A failure is
// reported and never fatal: the TCP view still works.
func (w *webServer) serveSocket(handler http.Handler) {
	dir, err := web.SocketDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "loghorn: -web: socket:", err)
		return
	}
	ln, path, err := web.ListenSocket(dir, os.Getpid())
	if err != nil {
		fmt.Fprintln(os.Stderr, "loghorn: -web: socket:", err)
		return
	}
	w.sockSrv = &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	w.sockPath = path
	go func() { _ = w.sockSrv.Serve(ln) }()
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
