package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/datadir"
	"github.com/jimbeveridge/loghorn/internal/incoming"
	"github.com/jimbeveridge/loghorn/internal/sock"
)

const statusUsage = `Usage: loghorn status [-env]

Show whether loghorn serve is running, where it listens and keeps its data,
any runs recorded while it was down, and the environment an OpenTelemetry
app needs to send to it. Exits 1 when serve isn't running. -env prints only
the export lines, for eval "$(loghorn status -env)".
`

// otelExports is the environment an OpenTelemetry SDK needs to send logs and
// traces to endpoint. Metrics are off: serve discards them.
func otelExports(endpoint string) string {
	return "export OTEL_EXPORTER_OTLP_ENDPOINT=" + endpoint + `
export OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
export OTEL_LOGS_EXPORTER=otlp
export OTEL_TRACES_EXPORTER=otlp
export OTEL_METRICS_EXPORTER=none
`
}

// otelEnv is otelExports with a heading and the per-app line.
func otelEnv(endpoint string) string {
	return "# OpenTelemetry clients: send logs and traces to loghorn\n" +
		otelExports(endpoint) +
		"# and per app: export OTEL_SERVICE_NAME=<your service>\n"
}

func cmdStatus(args []string, env cliEnv) int {
	fs := newFlagSet("status", statusUsage, env)
	envOnly := fs.Bool("env", false, "print only the OpenTelemetry export lines")
	pos, code, ok := parseCmd(fs, args, true)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		return usageError(env, "status takes no arguments")
	}
	path := sock.DefaultPath()
	sess, err := sock.Probe(path)
	up := err == nil

	endpoint := ""
	if up {
		endpoint = sess.OTLPEndpoint()
	} else {
		cfg, _, cerr := config.LoadUser()
		if cerr != nil {
			fmt.Fprintln(env.stderr, "loghorn:", cerr)
			return 2
		}
		_, port := cfg.Serve.Resolved()
		endpoint = "http://127.0.0.1:" + strconv.Itoa(port)
	}
	if *envOnly {
		fmt.Fprint(env.stdout, otelExports(endpoint))
		return 0
	}

	dataDir := sess.DataDir
	if up {
		fmt.Fprintf(env.stdout, "serve: running (pid %d, since %s, version %s)\n",
			sess.PID, sess.Started.Local().Format("2006-01-02 15:04:05"), sess.Version)
		fmt.Fprintf(env.stdout, "data: %s\nsocket: %s\nweb: %s\notlp/http: %s\n",
			sess.DataDir, sess.Socket, sess.WebURL(), sess.OTLPEndpoint())
	} else {
		if errors.Is(err, sock.ErrNotRunning) {
			fmt.Fprintln(env.stderr, "loghorn: "+notRunningMessage(path))
		} else {
			fmt.Fprintf(env.stderr, "loghorn: serve on %s: %v\n", path, err)
		}
		dataDir, _ = datadir.Dir()
	}
	if line := incomingSummary(dataDir); line != "" {
		fmt.Fprintln(env.stdout, line)
	}
	fmt.Fprintln(env.stdout)
	fmt.Fprint(env.stdout, otelEnv(endpoint))
	if !up {
		return 1
	}
	return 0
}

// incomingSummary describes the runs recorded while serve was down, or "".
func incomingSummary(dataDir string) string {
	if dataDir == "" {
		return ""
	}
	list, err := incoming.List(dataDir)
	if err != nil || len(list) == 0 {
		return ""
	}
	runs := "runs"
	if len(list) == 1 {
		runs = "run"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "incoming: %d %s recorded while serve was down", len(list), runs)
	if t := list[0].Meta.Started; !t.IsZero() {
		fmt.Fprintf(&b, ", oldest %s", t.Local().Format(stampFor(t)))
	}
	return b.String()
}

// stampFor shows a time of day for today, and the date otherwise.
func stampFor(t time.Time) string {
	if y, m, d := t.Local().Date(); time.Now().Year() == y && time.Now().Month() == m && time.Now().Day() == d {
		return "15:04"
	}
	return "2006-01-02 15:04"
}
