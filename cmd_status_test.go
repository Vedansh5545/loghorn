package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/incoming"
	"github.com/jimbeveridge/loghorn/internal/serve"
	"github.com/jimbeveridge/loghorn/internal/sock"
)

const wantEnv4318 = `# OpenTelemetry clients: send logs and traces to loghorn
export OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318
export OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
export OTEL_LOGS_EXPORTER=otlp
export OTEL_TRACES_EXPORTER=otlp
export OTEL_METRICS_EXPORTER=none
# and per app: export OTEL_SERVICE_NAME=<your service>
`

func TestOtelEnvText(t *testing.T) {
	if got := otelEnv("http://127.0.0.1:4318"); got != wantEnv4318 {
		t.Fatalf("otelEnv =\n%s", got)
	}
	exports := strings.Join(strings.Split(wantEnv4318, "\n")[1:6], "\n") + "\n"
	if got := otelExports("http://127.0.0.1:4318"); got != exports {
		t.Fatalf("otelExports =\n%s", got)
	}
}

// inProcessServe starts a serve in this process for status and ui tests and
// points LOGHORN_SOCKET, LOGHORN_DATA_DIR and HOME at its temp dirs.
func inProcessServe(t *testing.T) *serve.Server {
	t.Helper()
	e := newServeEnv(t)
	t.Setenv("HOME", e.home)
	t.Setenv("LOGHORN_DATA_DIR", e.data)
	t.Setenv("LOGHORN_SOCKET", e.socket)
	s, err := serve.Start(serve.Options{
		DataDir: e.data, Socket: e.socket, Config: config.Default(),
		ConfigBase: t.TempDir(), Version: "test", Log: &bytes.Buffer{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStatusUp(t *testing.T) {
	s := inProcessServe(t)
	held, heldSpans, err := incoming.Create(os.Getenv("LOGHORN_DATA_DIR"), incoming.Meta{RunID: "r1", Started: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	defer heldSpans.Close()
	out, errOut, code := dispatchIn(t, devNull(t), "status")
	sess := s.Session()
	for _, want := range []string{
		"serve: running (pid " + strconv.Itoa(sess.PID),
		"web: " + sess.WebURL(),
		"data: " + sess.DataDir,
		"incoming: 1 run recorded while serve was down",
		"export OTEL_EXPORTER_OTLP_ENDPOINT=" + sess.OTLPEndpoint(),
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("status lacks %q:\n%s%s", want, out, errOut)
		}
	}
	if code != 0 {
		t.Fatalf("code %d", code)
	}
}

func TestStatusDown(t *testing.T) {
	e := newServeEnv(t)
	t.Setenv("HOME", e.home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("LOGHORN_DATA_DIR", e.data)
	t.Setenv("LOGHORN_SOCKET", e.socket)
	p := filepath.Join(e.home, ".config", "loghorn", "config.toml")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("[serve]\notlp-http-port = 4999\n"), 0o644)
	out, errOut, code := dispatchIn(t, devNull(t), "status")
	if code != 1 || !strings.Contains(errOut, "loghorn serve isn't running") || !strings.Contains(out, "export OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4999\n") {
		t.Fatalf("code %d stdout %q stderr %q", code, out, errOut)
	}
	out, _, code = dispatchIn(t, devNull(t), "status", "-env")
	if code != 0 || out != otelExports("http://127.0.0.1:4999") {
		t.Fatalf("-env down: code %d stdout %q", code, out)
	}
}

func TestStatusEnvUp(t *testing.T) {
	s := inProcessServe(t)
	out, _, code := dispatchIn(t, devNull(t), "status", "-env")
	if code != 0 || out != otelExports(s.Session().OTLPEndpoint()) {
		t.Fatalf("code %d stdout %q", code, out)
	}
}

func TestUI(t *testing.T) {
	s := inProcessServe(t)
	var opened []string
	old := openBrowser
	t.Cleanup(func() { openBrowser = old })
	openBrowser = func(u string) error { opened = append(opened, u); return nil }

	_, errOut, code := dispatchIn(t, devNull(t), "ui")
	if code != 0 || len(opened) != 1 || opened[0] != s.Session().WebURL() || !strings.Contains(errOut, opened[0]) {
		t.Fatalf("ui: code %d opened %q stderr %q", code, opened, errOut)
	}
	out, _, code := dispatchIn(t, devNull(t), "ui", "-print")
	if code != 0 || out != s.Session().WebURL()+"\n" || len(opened) != 1 {
		t.Fatalf("ui -print: code %d stdout %q opened %q", code, out, opened)
	}
	openBrowser = func(string) error { return errors.New("no display") }
	_, errOut, code = dispatchIn(t, devNull(t), "ui")
	if code != 1 || !strings.Contains(errOut, "can't open a browser (no display); open "+s.Session().WebURL()+" yourself") {
		t.Fatalf("failing opener: code %d stderr %q", code, errOut)
	}
}

func TestUIServeDown(t *testing.T) {
	e := newServeEnv(t)
	t.Setenv("LOGHORN_SOCKET", e.socket)
	_, errOut, code := dispatchIn(t, devNull(t), "ui")
	if code != 1 || !strings.Contains(errOut, sock.ErrNotRunning.Error()) {
		t.Fatalf("code %d stderr %q", code, errOut)
	}
}
