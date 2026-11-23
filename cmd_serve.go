package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/datadir"
	"github.com/jimbeveridge/loghorn/internal/logfile"
	"github.com/jimbeveridge/loghorn/internal/serve"
	"github.com/jimbeveridge/loghorn/internal/sock"
	"github.com/jimbeveridge/loghorn/internal/svcconf"
)

const serveUsage = `Usage: loghorn serve [flags]

Run the store until SIGTERM or Ctrl-C: receive OTLP logs and traces from apps
and the records 'loghorn run' sends, keep them in the data folder, and serve
the web explorer. It stays in the foreground; start it from a terminal or
install it as a login service. Its own log goes to stderr (or -log).
`

func cmdServe(args []string, env cliEnv) int {
	fs := newFlagSet("serve", serveUsage, env)
	data := fs.String("data", "", "data folder (default $"+datadir.EnvVar+", else the per-user folder)")
	socket := fs.String("socket", "", "unix socket clients find serve by (default $"+sock.EnvVar+", else /tmp/loghorn.sock)")
	webPort := fs.Int("web-port", -1, fmt.Sprintf("web explorer port (default [serve] web-port, else %d; 0 picks a free one)", config.DefaultWebPort))
	otlpPort := fs.Int("otlp-http-port", -1, fmt.Sprintf("OTLP/HTTP port (default [serve] otlp-http-port, else %d; 0 picks a free one)", config.DefaultOTLPHTTPPort))
	logPath := fs.String("log", "", "append serve's own log to this file instead of stderr")
	pos, code, ok := parseCmd(fs, args, true)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		return usageError(env, "serve takes no arguments; got %q", pos)
	}

	cfg, cfgPath, err := config.LoadUser()
	if err != nil {
		fmt.Fprintln(env.stderr, "loghorn serve:", err)
		return 2
	}
	if *data == "" {
		if *data, err = datadir.Dir(); err != nil {
			fmt.Fprintln(env.stderr, "loghorn serve:", err)
			return 1
		}
	}
	if *socket == "" {
		*socket = sock.DefaultPath()
	}
	wp, op := cfg.Serve.Resolved()
	if *webPort >= 0 {
		wp = *webPort
	}
	if *otlpPort >= 0 {
		op = *otlpPort
	}
	log := env.stderr
	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			fmt.Fprintln(env.stderr, "loghorn serve:", err)
			return 1
		}
		defer f.Close()
		log = f
	}
	base, err := svcconf.BaseFromEnv()
	if err != nil {
		fmt.Fprintln(log, "loghorn serve: config dir:", err)
		return 1
	}

	srv, err := serve.Start(serve.Options{
		DataDir: *data, Socket: *socket, WebPort: wp, OTLPPort: op,
		Config: cfg, ConfigBase: base, Version: version(), Log: log,
	})
	if err != nil {
		fmt.Fprintln(log, "loghorn serve: "+startError(err, cfgPath))
		return 1
	}
	sess := srv.Session()
	fmt.Fprintf(log, "loghorn serve: web %s  otlp %s  socket %s  data %s\n",
		sess.WebURL(), sess.OTLPEndpoint(), sess.Socket, sess.DataDir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	<-ctx.Done()
	stop()
	if err := srv.Close(); err != nil {
		fmt.Fprintln(log, "loghorn serve: closing:", err)
		return 1
	}
	fmt.Fprintln(log, "loghorn serve: stopped")
	return 0
}

// startError words serve.Start's failure for the person reading the log.
func startError(err error, cfgPath string) string {
	if cfgPath == "" {
		cfgPath = userConfigPath()
	}
	var pe *serve.PortError
	var live *sock.LiveError
	var locked *logfile.LockedError
	switch {
	case errors.As(err, &pe):
		flagName, key, what := "-web-port", "web-port", "web"
		if pe.Which == "otlp-http" {
			flagName, key, what = "-otlp-http-port", "otlp-http-port", "OTLP"
		}
		return fmt.Sprintf("%s is in use (%v); choose another %s port with %s or [serve] %s in %s",
			pe.Addr, pe.Err, what, flagName, key, cfgPath)
	case errors.As(err, &live):
		return fmt.Sprintf("another serve is answering on %s; stop it, or give this one its own -socket, -data and ports", live.Path)
	case errors.As(err, &locked):
		return locked.Error()
	}
	return err.Error()
}

// userConfigPath is where the user-level config file would be, for messages.
func userConfigPath() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "loghorn", "config.toml")
	}
	return "~/.config/loghorn/config.toml"
}

// version is the module version the binary was built from, or "devel".
func version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "devel"
}

// cmdConfig prints the config file in effect for the current directory and
// the settings it resolves to.
func cmdConfig(args []string, env cliEnv) int {
	fs := newFlagSet("config", "Usage: loghorn config\n\nPrint the config file in effect for the current directory and the settings it\nresolves to.\n", env)
	pos, code, ok := parseCmd(fs, args, true)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		return usageError(env, "config takes no arguments")
	}
	cfg, path, _, code, ok := loadRepoConfig(env)
	if !ok {
		return code
	}
	if path == "" {
		fmt.Fprintln(env.stdout, "no config file found; using defaults")
	} else {
		fmt.Fprintln(env.stdout, path)
	}
	fmt.Fprint(env.stdout, cfg.String())
	return 0
}

// loadRepoConfig loads the config that applies to the current directory: the
// nearest repo .config/loghorn/config.toml, else the user-level file. A cwd
// that can't be resolved gets the defaults with a warning; a bad file is a
// usage error.
func loadRepoConfig(env cliEnv) (cfg config.Config, path, cwd string, code int, ok bool) {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(env.stderr, "loghorn: can't resolve the working directory, using default settings:", err)
		return config.Default(), "", "", 0, true
	}
	cfg, path, err = config.Load(cwd)
	if err != nil {
		fmt.Fprintln(env.stderr, "loghorn:", err)
		return config.Config{}, "", "", 2, false
	}
	return cfg, path, cwd, 0, true
}
