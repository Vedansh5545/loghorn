package otlp

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/adapter"
	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/engine"
	"github.com/jimbeveridge/loghorn/internal/record"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	respb "go.opentelemetry.io/proto/otlp/resource/v1"
)

func sv(s string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
}
func iv(i int64) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: i}}
}
func kv(k string, v *commonpb.AnyValue) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: v}
}
func kvl(kvs ...*commonpb.KeyValue) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: kvs}}}
}

func req(res []*commonpb.KeyValue, scope *commonpb.InstrumentationScope, recs ...*logspb.LogRecord) *collogspb.ExportLogsServiceRequest {
	return &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource:  &respb.Resource{Attributes: res},
		ScopeLogs: []*logspb.ScopeLogs{{Scope: scope, LogRecords: recs}},
	}}}
}

var now = time.Date(2030, 1, 2, 3, 4, 5, 6, time.UTC)

func decode(t *testing.T, line []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("bad json %s: %v", line, err)
	}
	return m
}

func one(t *testing.T, r *collogspb.ExportLogsServiceRequest) map[string]any {
	t.Helper()
	lines := MapLogs(r, now)
	if len(lines) != 1 {
		t.Fatalf("got %d lines", len(lines))
	}
	return decode(t, lines[0])
}

func rec(lr *logspb.LogRecord) *collogspb.ExportLogsServiceRequest { return req(nil, nil, lr) }

func TestStringBody(t *testing.T) {
	m := one(t, rec(&logspb.LogRecord{Body: sv("hello")}))
	if m["message"] != "hello" || m["loghorn.source"] != "otlp" {
		t.Fatalf("%v", m)
	}
	if !reflect.DeepEqual(m["labels"], map[string]any{"schema": "otel"}) {
		t.Fatalf("labels %v", m["labels"])
	}
}

func TestMapBody(t *testing.T) {
	m := one(t, rec(&logspb.LogRecord{Body: kvl(kv("msg", sv("m")), kv("user", sv("u")), kv("severity", sv("x")))}))
	if m["message"] != "m" || m["user"] != "u" || m["otel.body.severity"] != "x" {
		t.Fatalf("%v", m)
	}
	if _, ok := m["msg"]; ok {
		t.Fatalf("msg kept: %v", m)
	}
	m = one(t, rec(&logspb.LogRecord{Body: kvl(kv("message", sv("a")), kv("msg", sv("b")))}))
	if m["message"] != "a" || m["msg"] != "b" {
		t.Fatalf("%v", m)
	}
}

func TestOtherBodyKind(t *testing.T) {
	m := one(t, rec(&logspb.LogRecord{Body: iv(7)}))
	if m["otel.body"] != json.Number("7") {
		t.Fatalf("%v", m)
	}
	if _, ok := m["message"]; ok {
		t.Fatalf("message set: %v", m)
	}
}

func TestAttributeValueTypes(t *testing.T) {
	attrs := []*commonpb.KeyValue{
		kv("s", sv("x")),
		kv("b", &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}),
		kv("i", iv(9007199254740993)),
		kv("d", &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 1.5}}),
		kv("nan", &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: math.NaN()}}),
		kv("pinf", &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: math.Inf(1)}}),
		kv("ninf", &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: math.Inf(-1)}}),
		kv("arr", &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{iv(1), sv("a")}}}}),
		kv("kv", kvl(kv("k", sv("v")))),
		kv("bytes", &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte{1, 2}}}),
		kv("empty", &commonpb.AnyValue{}),
		kv("http.response.status_code", iv(200)),
	}
	m := one(t, rec(&logspb.LogRecord{Attributes: attrs}))
	want := map[string]any{
		"s": "x", "b": true, "i": json.Number("9007199254740993"), "d": json.Number("1.5"),
		"nan": "NaN", "pinf": "+Inf", "ninf": "-Inf",
		"arr": []any{json.Number("1"), "a"}, "kv": map[string]any{"k": "v"},
		"bytes": "AQI=", "empty": nil, "http.response.status_code": json.Number("200"),
	}
	for k, w := range want {
		g, ok := m[k]
		if !ok || !reflect.DeepEqual(g, w) {
			t.Errorf("%s = %#v (present=%v), want %#v", k, g, ok, w)
		}
	}
}

func TestSeverity(t *testing.T) {
	cases := []struct {
		text string
		num  logspb.SeverityNumber
		want string // "" = absent
	}{
		{"WARN", 13, "WARNING"},
		{"error", 0, "ERROR"},
		{"", 3, "DEBUG"},
		{"", 9, "INFO"},
		{"", 17, "ERROR"},
		{"", 21, "CRITICAL"},
		{"Verbose", 0, "Verbose"},
		{"default", 0, "DEFAULT"},
		{"", 0, ""},
	}
	for _, c := range cases {
		m := one(t, rec(&logspb.LogRecord{SeverityText: c.text, SeverityNumber: c.num}))
		got, ok := m["severity"]
		if c.want == "" {
			if ok {
				t.Errorf("%q/%d: severity present %v", c.text, c.num, got)
			}
		} else if got != c.want {
			t.Errorf("%q/%d: severity %v, want %s", c.text, c.num, got, c.want)
		}
	}
}

func TestTimestamp(t *testing.T) {
	m := one(t, rec(&logspb.LogRecord{TimeUnixNano: 1763584812123456789}))
	if m["timestamp"] != "2025-11-19T20:40:12.123456789Z" {
		t.Fatalf("%v", m["timestamp"])
	}
	m = one(t, rec(&logspb.LogRecord{ObservedTimeUnixNano: 1763584812123456789}))
	if m["timestamp"] != "2025-11-19T20:40:12.123456789Z" {
		t.Fatalf("%v", m["timestamp"])
	}
	m = one(t, rec(&logspb.LogRecord{}))
	if m["timestamp"] != now.Format(time.RFC3339Nano) {
		t.Fatalf("%v", m["timestamp"])
	}
}

func TestTraceIDs(t *testing.T) {
	m := one(t, rec(&logspb.LogRecord{TraceId: make([]byte, 16)}))
	if _, ok := m["trace_id"]; ok {
		t.Fatal("zero trace id kept")
	}
	if _, ok := m["span_id"]; ok {
		t.Fatal("empty span id kept")
	}
	tid := []byte{0xAB, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 0xCD}
	m = one(t, rec(&logspb.LogRecord{TraceId: tid, SpanId: []byte{0xEF, 0, 0, 0, 0, 0, 0, 1}}))
	if m["trace_id"] != "ab0102030405060708090a0b0c0d0ecd" || m["span_id"] != "ef00000000000001" {
		t.Fatalf("%v %v", m["trace_id"], m["span_id"])
	}
}

func TestResourceAndScope(t *testing.T) {
	res := []*commonpb.KeyValue{kv("service.name", sv("api")), kv("loghorn.run.id", sv("R")), kv("host.name", sv("h"))}
	m := one(t, req(res, &commonpb.InstrumentationScope{Name: "lib", Version: "1.2"}, &logspb.LogRecord{}))
	if m["service.name"] != "api" || m["loghorn.run.id"] != "R" ||
		m["otel.scope.name"] != "lib" || m["otel.scope.version"] != "1.2" ||
		!reflect.DeepEqual(m["otel.resource"], map[string]any{"host.name": "h"}) {
		t.Fatalf("%v", m)
	}
	m = one(t, req(res[:1], nil, &logspb.LogRecord{}))
	if _, ok := m["otel.resource"]; ok {
		t.Fatalf("otel.resource present: %v", m)
	}
	if _, ok := m["otel.scope.name"]; ok {
		t.Fatalf("scope name present: %v", m)
	}
}

func TestCollisions(t *testing.T) {
	m := one(t, rec(&logspb.LogRecord{
		SeverityText: "INFO",
		Body:         kvl(kv("user", sv("body"))),
		Attributes: []*commonpb.KeyValue{
			kv("severity", sv("a")), kv("otel.resource", sv("b")),
			kv("labels.schema", sv("c")), kv("user", sv("d")),
		},
	}))
	want := map[string]any{
		"otel.attributes.severity": "a", "otel.attributes.otel.resource": "b",
		"otel.attributes.labels.schema": "c", "otel.attributes.user": "d",
		"user": "body", "severity": "INFO",
	}
	for k, w := range want {
		if m[k] != w {
			t.Errorf("%s = %v, want %v", k, m[k], w)
		}
	}
	if !reflect.DeepEqual(m["labels"], map[string]any{"schema": "otel"}) {
		t.Errorf("labels %v", m["labels"])
	}
	if _, ok := m["otel.resource"]; ok {
		t.Errorf("otel.resource overwritten")
	}
}

func TestStreamFallbacks(t *testing.T) {
	ev := kv("event.name", sv("x"))
	lib := &commonpb.InstrumentationScope{Name: "lib"}
	cases := []struct {
		lr    *logspb.LogRecord
		scope *commonpb.InstrumentationScope
		want  string
	}{
		{&logspb.LogRecord{EventName: "app.db", Attributes: []*commonpb.KeyValue{ev}}, nil, "app.db"},
		{&logspb.LogRecord{Attributes: []*commonpb.KeyValue{ev}}, lib, "x"},
		{&logspb.LogRecord{}, lib, "lib"},
		{&logspb.LogRecord{}, nil, "otel"},
		{&logspb.LogRecord{EventName: "", Attributes: []*commonpb.KeyValue{ev}}, nil, "x"},
	}
	for i, c := range cases {
		if got := Stream(c.lr, c.scope); got != c.want {
			t.Errorf("case %d: %q, want %q", i, got, c.want)
		}
	}
}

func TestMappedLineNormalizes(t *testing.T) {
	ts := uint64(1763584812123456789)
	lr := &logspb.LogRecord{
		EventName: "app.db", SeverityNumber: 17, TimeUnixNano: ts, Body: sv("boom"),
		Attributes: []*commonpb.KeyValue{kv("http.response.status_code", iv(503))},
	}
	lines := MapLogs(req([]*commonpb.KeyValue{kv("service.name", sv("api"))}, nil, lr), now)
	e := adapter.ParseLine(lines[0])
	r, ok := record.Normalize(e, config.Default().Input)
	if !ok {
		t.Fatalf("not normalized: %s", lines[0])
	}
	if r.Stream != "app.db" {
		t.Errorf("stream %q", r.Stream)
	}
	if r.Fields["service.name"] != "api" {
		t.Errorf("service.name %v", r.Fields["service.name"])
	}
	if r.Fields["http.response.status_code"] != json.Number("503") {
		t.Errorf("status %#v", r.Fields["http.response.status_code"])
	}
	if r.Fields["severity"] != "ERROR" {
		t.Errorf("severity %v", r.Fields["severity"])
	}
	if !r.Time.Equal(time.Unix(0, int64(ts))) {
		t.Errorf("time %v", r.Time)
	}
	if !engine.IsImportant(e) {
		t.Errorf("not important")
	}
}

func TestMapLogsOrder(t *testing.T) {
	var rls []*logspb.ResourceLogs
	n := 0
	for r := 0; r < 2; r++ {
		rl := &logspb.ResourceLogs{}
		for s := 0; s < 2; s++ {
			sl := &logspb.ScopeLogs{}
			for i := 0; i < 2; i++ {
				n++
				sl.LogRecords = append(sl.LogRecords, &logspb.LogRecord{Body: iv(int64(n)), Attributes: []*commonpb.KeyValue{kv("n", iv(int64(n)))}})
			}
			rl.ScopeLogs = append(rl.ScopeLogs, sl)
		}
		rls = append(rls, rl)
	}
	lines := MapLogs(&collogspb.ExportLogsServiceRequest{ResourceLogs: rls}, now)
	if len(lines) != 8 {
		t.Fatalf("%d lines", len(lines))
	}
	for i, l := range lines {
		if got := decode(t, l)["n"]; got != json.Number(string(rune('1'+i))) {
			t.Errorf("line %d n=%v", i, got)
		}
	}
}
