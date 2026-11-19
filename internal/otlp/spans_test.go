package otlp

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func traceReq(res []*commonpb.KeyValue, scope *commonpb.InstrumentationScope, spans ...*tracepb.Span) *coltracepb.ExportTraceServiceRequest {
	return &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource:   &resourcepb.Resource{Attributes: res},
		ScopeSpans: []*tracepb.ScopeSpans{{Scope: scope, Spans: spans}},
	}}}
}

func oneSpan(t *testing.T, r *coltracepb.ExportTraceServiceRequest) map[string]any {
	t.Helper()
	lines := MapSpans(r)
	if len(lines) != 1 {
		t.Fatalf("got %d lines", len(lines))
	}
	return decode(t, lines[0])
}

func seq(from byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = from + byte(i)
	}
	return b
}

func TestMapSpanFull(t *testing.T) {
	m := oneSpan(t, traceReq(
		[]*commonpb.KeyValue{kv("service.name", sv("api")), kv("loghorn.run.id", sv("R")), kv("host.name", sv("h"))},
		&commonpb.InstrumentationScope{Name: "lib", Version: "1.2"},
		&tracepb.Span{
			TraceId:           seq(1, 16),
			SpanId:            seq(0xa1, 8),
			ParentSpanId:      seq(0xb1, 8),
			Name:              "GET /x",
			Kind:              tracepb.Span_SPAN_KIND_SERVER,
			StartTimeUnixNano: 1763584812000000000,
			EndTimeUnixNano:   1763584812001500000,
			Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "boom"},
			Attributes:        []*commonpb.KeyValue{kv("http.response.status_code", iv(500))},
		}))
	want := map[string]any{
		"trace_id":           "0102030405060708090a0b0c0d0e0f10",
		"span_id":            "a1a2a3a4a5a6a7a8",
		"parent_span_id":     "b1b2b3b4b5b6b7b8",
		"name":               "GET /x",
		"kind":               "server",
		"start":              "2025-11-19T20:40:12Z",
		"end":                "2025-11-19T20:40:12.0015Z",
		"duration_ms":        json.Number("1.5"),
		"status":             "error",
		"status_message":     "boom",
		"attributes":         map[string]any{"http.response.status_code": json.Number("500")},
		"service.name":       "api",
		"loghorn.run.id":     "R",
		"otel.resource":      map[string]any{"host.name": "h"},
		"otel.scope.name":    "lib",
		"otel.scope.version": "1.2",
	}
	if !reflect.DeepEqual(m, want) {
		t.Errorf("got  %v\nwant %v", m, want)
	}
}

func TestMapSpanKeyOrder(t *testing.T) {
	lines := MapSpans(traceReq(
		[]*commonpb.KeyValue{kv("service.name", sv("api"))}, nil,
		&tracepb.Span{TraceId: seq(1, 16), SpanId: seq(2, 8), Name: "n", Kind: tracepb.Span_SPAN_KIND_CLIENT}))
	if len(lines) != 1 || !strings.HasPrefix(string(lines[0]), `{"trace_id":"`) ||
		!strings.Contains(string(lines[0]), `"name":"n","kind":"client"`) {
		t.Errorf("line = %s", lines)
	}
}

func TestMapSpanRootAndUnset(t *testing.T) {
	m := oneSpan(t, traceReq(
		[]*commonpb.KeyValue{kv("service.name", sv("api"))}, nil,
		&tracepb.Span{TraceId: seq(1, 16), SpanId: seq(2, 8), ParentSpanId: make([]byte, 8), Name: "root",
			Status: &tracepb.Status{}}))
	for _, k := range []string{"parent_span_id", "kind", "status", "status_message", "attributes", "events", "links", "otel.resource", "otel.scope.name", "otel.scope.version"} {
		if v, ok := m[k]; ok {
			t.Errorf("%s = %v, want absent", k, v)
		}
	}
	if m["name"] != "root" || m["service.name"] != "api" {
		t.Errorf("got %v", m)
	}
}

func TestMapSpanMissingIDsKept(t *testing.T) {
	m := oneSpan(t, traceReq(nil, nil, &tracepb.Span{}))
	for _, k := range []string{"trace_id", "span_id", "name"} {
		if v, ok := m[k]; !ok || v != "" {
			t.Errorf("%s = %v (present %v), want empty string", k, v, ok)
		}
	}
}

func TestMapSpanKinds(t *testing.T) {
	for kind, want := range map[tracepb.Span_SpanKind]string{
		tracepb.Span_SPAN_KIND_INTERNAL: "internal",
		tracepb.Span_SPAN_KIND_SERVER:   "server",
		tracepb.Span_SPAN_KIND_CLIENT:   "client",
		tracepb.Span_SPAN_KIND_PRODUCER: "producer",
		tracepb.Span_SPAN_KIND_CONSUMER: "consumer",
	} {
		if got := oneSpan(t, traceReq(nil, nil, &tracepb.Span{Kind: kind}))["kind"]; got != want {
			t.Errorf("%v → %v, want %s", kind, got, want)
		}
	}
	if got, ok := oneSpan(t, traceReq(nil, nil, &tracepb.Span{Kind: 42}))["kind"]; ok {
		t.Errorf("unknown kind → %v, want absent", got)
	}
}

func TestMapSpanStatusOK(t *testing.T) {
	m := oneSpan(t, traceReq(nil, nil, &tracepb.Span{Status: &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK}}))
	if m["status"] != "ok" {
		t.Errorf("status = %v", m["status"])
	}
	m = oneSpan(t, traceReq(nil, nil, &tracepb.Span{Status: &tracepb.Status{Message: "m"}}))
	if _, ok := m["status"]; ok || m["status_message"] != "m" {
		t.Errorf("unset with message: %v", m)
	}
}

func TestMapSpanTimes(t *testing.T) {
	m := oneSpan(t, traceReq(nil, nil, &tracepb.Span{EndTimeUnixNano: 1763584812000000000}))
	if _, ok := m["start"]; ok {
		t.Errorf("start present: %v", m)
	}
	if _, ok := m["duration_ms"]; ok {
		t.Errorf("duration_ms present without start: %v", m)
	}
	if m["end"] != "2025-11-19T20:40:12Z" {
		t.Errorf("end = %v", m["end"])
	}
	m = oneSpan(t, traceReq(nil, nil, &tracepb.Span{StartTimeUnixNano: 1763584812000000000, EndTimeUnixNano: 1763584811000000000}))
	if m["start"] != "2025-11-19T20:40:12Z" || m["end"] != "2025-11-19T20:40:11Z" {
		t.Errorf("times = %v / %v", m["start"], m["end"])
	}
	if _, ok := m["duration_ms"]; ok {
		t.Errorf("duration_ms present with end < start: %v", m)
	}
	m = oneSpan(t, traceReq(nil, nil, &tracepb.Span{StartTimeUnixNano: 1763584812000000000, EndTimeUnixNano: 1763584812000000000}))
	if m["duration_ms"] != json.Number("0") {
		t.Errorf("zero-length span duration_ms = %v", m["duration_ms"])
	}
}

func TestMapSpanEventsAndLinks(t *testing.T) {
	stack := strings.Repeat("x", 10000)
	m := oneSpan(t, traceReq(nil, nil, &tracepb.Span{
		Events: []*tracepb.Span_Event{
			{TimeUnixNano: 1763584812000000000, Name: "exception", Attributes: []*commonpb.KeyValue{
				kv("exception.type", sv("E")), kv("exception.message", sv("bad")), kv("exception.stacktrace", sv(stack)),
			}},
			{Name: "bare"},
		},
		Links: []*tracepb.Span_Link{{TraceId: seq(1, 16), SpanId: seq(0xa1, 8), Attributes: []*commonpb.KeyValue{kv("k", sv("v"))}}},
	}))
	events, _ := m["events"].([]any)
	if len(events) != 2 {
		t.Fatalf("events = %v", m["events"])
	}
	wantEv := map[string]any{
		"time": "2025-11-19T20:40:12Z", "name": "exception",
		"attributes": map[string]any{"exception.type": "E", "exception.message": "bad", "exception.stacktrace": stack},
	}
	if !reflect.DeepEqual(events[0], wantEv) {
		t.Errorf("events[0] = %v", events[0])
	}
	if !reflect.DeepEqual(events[1], map[string]any{"name": "bare"}) {
		t.Errorf("events[1] = %v, want only a name", events[1])
	}
	links, _ := m["links"].([]any)
	wantLink := map[string]any{"trace_id": "0102030405060708090a0b0c0d0e0f10", "span_id": "a1a2a3a4a5a6a7a8", "attributes": map[string]any{"k": "v"}}
	if len(links) != 1 || !reflect.DeepEqual(links[0], wantLink) {
		t.Errorf("links = %v", m["links"])
	}
}

func TestMapSpansOrder(t *testing.T) {
	r := &coltracepb.ExportTraceServiceRequest{}
	n := 0
	for i := 0; i < 2; i++ {
		rs := &tracepb.ResourceSpans{}
		for j := 0; j < 2; j++ {
			ss := &tracepb.ScopeSpans{}
			for k := 0; k < 2; k++ {
				ss.Spans = append(ss.Spans, &tracepb.Span{Name: fmt.Sprintf("s%d", n)})
				n++
			}
			rs.ScopeSpans = append(rs.ScopeSpans, ss)
		}
		r.ResourceSpans = append(r.ResourceSpans, rs)
	}
	lines := MapSpans(r)
	if len(lines) != 8 {
		t.Fatalf("got %d lines", len(lines))
	}
	for i, l := range lines {
		if strings.HasSuffix(string(l), "\n") {
			t.Errorf("line %d ends in a newline", i)
		}
		if got := decode(t, l)["name"]; got != fmt.Sprintf("s%d", i) {
			t.Errorf("line %d name = %v", i, got)
		}
	}
}
