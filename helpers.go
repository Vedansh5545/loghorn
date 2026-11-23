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

// webServer is open's temporary view; the zero value (nil) is "no view".
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

// startView listens on port (0 picks a free one) and serves an empty store of
// the given mode for open to fill; it does not print the URL. It has no
// socket: only serve is found that way, and query reaches this view by its
// full URL. A config base or listen failure exits; open launches nothing, so
// there is nothing to clean up.
func startView(port int, cwd string, mode store.Mode) *webServer {
	base, err := svcconf.BaseFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "loghorn: config dir:", err)
		os.Exit(1)
	}
	if msg := repoConfigNotice(cwd, base); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
	}
	st := store.New(mode, time.Time{})
	ln, err := web.Listen(port)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loghorn:", err)
		os.Exit(1)
	}
	handler := web.New(web.Options{
		Store: st, ConfigBase: base,
		PID: os.Getpid(), Cwd: cwd, Port: ln.Addr().(*net.TCPAddr).Port, Started: time.Now(),
	})
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
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
