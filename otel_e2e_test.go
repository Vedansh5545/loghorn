package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
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
	if err := postSpan(); err != nil {
		fmt.Fprintln(os.Stderr, "traces:", err)
		return 97
	}
	fmt.Println(`{"severity":"INFO","message":"after","labels":{"schema":"app"}}`)
	fmt.Println("plain after")
	return 3
}

// postSpan sends one span to the traces endpoint loghorn set, as protobuf.
// The repo has no OTEL trace SDK exporter, so the request is built by hand.
func postSpan() error {
	str := func(k, v string) *commonpb.KeyValue {
		return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
	}
	start := time.Now()
	req := &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			str("service.name", "e2e"), str("loghorn.run.id", os.Getenv("LOGHORN_RUN_ID")),
		}},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{
			TraceId:           []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
			SpanId:            []byte{1, 2, 3, 4, 5, 6, 7, 8},
			Name:              "e2e.span",
			Kind:              tracepb.Span_SPAN_KIND_SERVER,
			StartTimeUnixNano: uint64(start.UnixNano()),
			EndTimeUnixNano:   uint64(start.Add(time.Millisecond).UnixNano()),
		}}}},
	}}}
	b, err := proto.Marshal(req)
	if err != nil {
		return err
	}
	resp, err := http.Post(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"), "application/x-protobuf", bytes.NewReader(b))
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// helperEnv makes the loghorn binary run name as its child (the environment
// passes through to the child).
func helperEnv(t *testing.T, name string) {
	t.Helper()
	t.Setenv("LOGHORN_TEST_HELPER", name)
	t.Setenv("OTEL_SERVICE_NAME", "e2e")
}

var runIDRe = regexp.MustCompile(`[0-9]{8}T[0-9]{6}Z-[0-9a-f]{4}`)
