package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// runHelper is the body of a child the -otel tests run: the test binary itself
// with LOGHORN_TEST_HELPER set. It returns the exit status.
func runHelper(name string) int {
	switch name {
	case "json-only":
		fmt.Println(`{"severity":"INFO","message":"lonely","labels":{"schema":"app"}}`)
		return 0
	case "otel-child":
		return otelChild()
	}
	fmt.Fprintln(os.Stderr, "unknown helper", name)
	return 99
}

func otelChild() int {
	fmt.Println(`{"severity":"INFO","message":"before","labels":{"schema":"app"}}`)
	fmt.Println("plain before")
	// Wait until loghorn has read the stdout lines, so the OTLP record cannot
	// overtake them.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile("./.loghorn/loghorn.log"); err == nil && strings.Contains(string(b), "plain before") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	ctx := context.Background()
	exp, err := otlploghttp.New(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "exporter:", err)
		return 98
	}
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)))
	// In otel/log v1.47 a Record takes attribute.Value/attribute.KeyValue
	// (attribute.StringValue, attribute.String/Int), not log.StringValue.
	var r otellog.Record
	r.SetTimestamp(time.Now())
	r.SetBody(attribute.StringValue("hello otel"))
	r.SetSeverity(otellog.SeverityError)
	r.SetEventName("test.event")
	r.AddAttributes(attribute.String("event.name", "test.event"), attribute.Int("http.response.status_code", 200))
	provider.Logger("e2e").Emit(ctx, r)
	_ = provider.ForceFlush(ctx)
	_ = provider.Shutdown(ctx)
	fmt.Println(`{"severity":"INFO","message":"after","labels":{"schema":"app"}}`)
	fmt.Println("plain after")
	return 3
}

// helperEnv makes the loghorn binary run name as its child (the environment
// passes through to the child).
func helperEnv(t *testing.T, name string) {
	t.Helper()
	t.Setenv("LOGHORN_TEST_HELPER", name)
	t.Setenv("OTEL_SERVICE_NAME", "e2e")
}

var runIDRe = regexp.MustCompile(`[0-9]{8}T[0-9]{6}Z-[0-9a-f]{4}`)

func TestOtelEndToEnd(t *testing.T) {
	helperEnv(t, "otel-child")
	dir := t.TempDir()
	res := runLoghorn(t, dir, nil, "-otel", "--", os.Args[0])
	if res.code != 3 {
		t.Fatalf("exit %d, want 3; stderr: %s", res.code, res.stderr)
	}
	last := -1
	for _, want := range []string{"before", "plain before", "after", "plain after"} {
		i := strings.Index(res.stdout[last+1:], want)
		if i < 0 {
			t.Fatalf("stdout lacks %q in order: %q", want, res.stdout)
		}
		last += 1 + i
	}
	if !strings.Contains(res.stderr, "loghorn: -otel: receiving OTLP at http://127.0.0.1:") {
		t.Errorf("stderr lacks the receiving line: %q", res.stderr)
	}
	id := runIDRe.FindString(res.stderr)
	if id == "" {
		t.Fatalf("stderr has no run id: %q", res.stderr)
	}
	if strings.Contains(res.stderr, "no OTLP logs received") {
		t.Errorf("stderr warns although OTLP arrived: %q", res.stderr)
	}
	logText := readLog(t, dir)
	for _, want := range []string{`{"severity":"INFO","message":"before","labels":{"schema":"app"}}`, "plain before", "plain after"} {
		if !strings.Contains(logText, want) {
			t.Errorf("log lacks %q:\n%s", want, logText)
		}
	}
	if strings.Contains(logText, `"message":"after"`) {
		t.Errorf("log kept the stdout JSON line after OTLP began:\n%s", logText)
	}
	var otlpLines []string
	for _, l := range strings.Split(logText, "\n") {
		if strings.Contains(l, `"loghorn.source":"otlp"`) {
			otlpLines = append(otlpLines, l)
		}
	}
	if len(otlpLines) != 1 {
		t.Fatalf("want 1 OTLP line, got %d:\n%s", len(otlpLines), logText)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(otlpLines[0]), &rec); err != nil {
		t.Fatalf("OTLP line: %v", err)
	}
	labels, _ := rec["labels"].(map[string]any)
	if labels["schema"] != "test.event" {
		t.Errorf("labels = %v", rec["labels"])
	}
	for k, want := range map[string]any{
		"message": "hello otel", "severity": "ERROR", "service.name": "e2e",
		"loghorn.run.id": id, "http.response.status_code": float64(200),
	} {
		if rec[k] != want {
			t.Errorf("%s = %v, want %v", k, rec[k], want)
		}
	}
}

func TestOtelNoLogsWarns(t *testing.T) {
	helperEnv(t, "json-only")
	dir := t.TempDir()
	res := runLoghorn(t, dir, nil, "-otel", "--", os.Args[0])
	if res.code != 0 {
		t.Fatalf("exit %d; stderr: %s", res.code, res.stderr)
	}
	if !strings.Contains(res.stderr, "loghorn: -otel: no OTLP logs received; stdout was recorded instead") {
		t.Errorf("stderr lacks the warning: %q", res.stderr)
	}
	if !strings.Contains(readLog(t, dir), "lonely") {
		t.Errorf("log lacks the stdout record")
	}
}

func TestOtelChildEnvironment(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "team=x")
	dir := t.TempDir()
	res := runLoghorn(t, dir, nil, "-otel", "--", "sh", "-c",
		`echo "$OTEL_EXPORTER_OTLP_LOGS_ENDPOINT|$OTEL_LOGS_EXPORTER|$LOGHORN_RUN_ID|$OTEL_RESOURCE_ATTRIBUTES"`)
	m := regexp.MustCompile(`^http://127\.0\.0\.1:\d+/v1/logs\|otlp\|(\S+)\|team=x,loghorn\.run\.id=(\S+)\n$`).FindStringSubmatch(res.stdout)
	if m == nil || m[1] != m[2] || !runIDRe.MatchString(m[1]) {
		t.Errorf("child environment: %q", res.stdout)
	}
}

func TestOtelRejectsPipe(t *testing.T) {
	res := runLoghorn(t, t.TempDir(), strings.NewReader("x\n"), "-otel")
	if res.code != 2 || !strings.Contains(res.stderr, "-otel needs a command after --; it can't be used with piped input") {
		t.Errorf("code %d, stderr %q", res.code, res.stderr)
	}
}
