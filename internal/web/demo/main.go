// Command demo serves the web query view over stored log files, for frontend
// work and manual checks:
//
//	go run ./internal/web/demo [-port 8790] FILE...
//
// Pins, favorites and field settings go to the user's config,
// $XDG_CONFIG_HOME/loghorn/services/<service>/ (else ~/.config/loghorn/...),
// so run it with a scratch config:
//
//	XDG_CONFIG_HOME=$(mktemp -d) go run ./internal/web/demo FILE...
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/store"
	"github.com/jimbeveridge/loghorn/internal/svcconf"
	"github.com/jimbeveridge/loghorn/internal/web"
)

func main() {
	port := flag.Int("port", 8790, "TCP port on 127.0.0.1 (0 picks a free one)")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: demo [-port N] FILE...")
		os.Exit(2)
	}
	st := store.New(store.ModeHistorical, time.Time{})
	if err := store.Preload(st, flag.Args(), config.Default().Input); err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
	base, err := svcconf.BaseFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
	l, err := web.Listen(*port)
	if err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
	srv := &http.Server{Handler: web.New(web.Options{Store: st, ConfigBase: base}), ReadHeaderTimeout: 10 * time.Second}
	fmt.Printf("loghorn demo: http://%s/ (%d records)\n", l.Addr(), st.Info().Count)
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt)
		<-ch
		srv.Close()
	}()
	if err := srv.Serve(l); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
}
