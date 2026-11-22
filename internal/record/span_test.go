package record

import (
	"bufio"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/adapter"
	"github.com/jimbeveridge/loghorn/internal/config"
)

func spanLines(t *testing.T) [][]byte {
	t.Helper()
	f, err := os.Open("../../testdata/web/spans.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		out = append(out, append([]byte(nil), sc.Bytes()...))
	}
	return out
}

func TestSpanStream(t *testing.T) {
	m := func(kv ...any) map[string]any {
		out := map[string]any{}
		for i := 0; i < len(kv); i += 2 {
			out[kv[i].(string)] = kv[i+1]
		}
		return out
	}
	cases := []struct {
		name  string
		kind  string
		attrs map[string]any
		want  string
	}{
		{"gen_ai", "client", m("gen_ai.operation.name", "chat"), StreamGenAI},
		{"messaging", "producer", m("messaging.system.name", "q"), StreamMessaging},
		{"messaging legacy", "producer", m("messaging.system", "q"), StreamMessaging},
		{"rpc", "client", m("rpc.system.name", "grpc"), StreamRPC},
		{"rpc legacy", "client", m("rpc.system", "grpc"), StreamRPC},
		{"db", "client", m("db.system.name", "sqlite"), StreamDB},
		{"db legacy", "client", m("db.system", "pg"), StreamDB},
		{"http.server", "server", m("http.request.method", "GET"), StreamHTTPServer},
		{"http.server legacy", "server", m("http.method", "GET"), StreamHTTPServer},
		{"http.client", "client", m("http.request.method", "GET"), StreamHTTPClient},
		{"http.client legacy", "client", m("http.method", "GET"), StreamHTTPClient},
		{"gen_ai over http", "client", m("gen_ai.request.model", "x", "http.request.method", "POST"), StreamGenAI},
		{"messaging over rpc", "producer", m("messaging.system.name", "q", "rpc.system.name", "grpc"), StreamMessaging},
		{"internal db", "internal", m("db.system.name", "sqlite"), StreamSpan},
		{"server db", "server", m("db.system.name", "sqlite"), StreamSpan},
		{"internal http", "internal", m("http.request.method", "GET"), StreamSpan},
		{"nil attrs", "client", nil, StreamSpan},
		{"nil value", "client", m("db.system.name", nil), StreamSpan},
	}
	for _, c := range cases {
		if got := SpanStream(c.kind, c.attrs); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestNormalizeSpanFixture(t *testing.T) {
	want := []string{"gen_ai", "messaging", "rpc", "db", "db open", "http.server", "http.client", "job.run", "db"}
	lines := spanLines(t)
	if len(lines) != len(want) {
		t.Fatalf("fixture has %d lines, want %d", len(lines), len(want))
	}
	for i, l := range lines {
		r, ok := NormalizeSpan(l)
		if !ok || r.Source != SourceOtel || r.Stream != want[i] {
			t.Errorf("line %d: ok=%v source=%q stream=%q, want %q", i+1, ok, r.Source, r.Stream, want[i])
		}
	}
}

func TestNormalizeSpanFields(t *testing.T) {
	lines := spanLines(t)
	r, _ := NormalizeSpan(lines[3])
	f := r.Fields
	if f["db.query.text"] != "select id from items where id = ?" {
		t.Errorf("db.query.text = %v", f["db.query.text"])
	}
	if _, ok := f["attributes.db.query.text"]; ok {
		t.Error("attribute key duplicated under attributes.")
	}
	if f["db.response.returned_rows"] != json.Number("3") || f["duration_ms"] != json.Number("8") {
		t.Errorf("numbers: %v %v", f["db.response.returned_rows"], f["duration_ms"])
	}
	if f["timestamp"] != "2026-11-17T17:01:05.000Z" || f["severity"] != "INFO" {
		t.Errorf("timestamp/severity: %v %v", f["timestamp"], f["severity"])
	}
	if !r.Time.Equal(time.Date(2026, 11, 17, 17, 1, 5, 0, time.UTC)) {
		t.Errorf("Time = %v", r.Time)
	}
	if string(r.Raw) != string(lines[3]) {
		t.Errorf("Raw = %s", r.Raw)
	}
	r1, _ := NormalizeSpan(lines[0])
	if r1.Fields["otel.resource.host.name"] != "host-1" || r1.Fields["service.name"] != "demo-svc" {
		t.Errorf("line 1 fields: %v", r1.Fields)
	}
	r8, _ := NormalizeSpan(lines[7])
	if r8.Fields["severity"] != "ERROR" {
		t.Errorf("severity = %v", r8.Fields["severity"])
	}
	if ev, ok := r8.Fields["events"].([]any); !ok || len(ev) != 1 {
		t.Errorf("events = %#v", r8.Fields["events"])
	}
	if r8.Fields["span.event_count"] != json.Number("1") || f["span.event_count"] != json.Number("0") {
		t.Errorf("span.event_count = %v, %v", r8.Fields["span.event_count"], f["span.event_count"])
	}
}

func TestNormalizeSpanAttributeCollision(t *testing.T) {
	r, _ := NormalizeSpan(spanLines(t)[7])
	if r.Fields["name"] != "job.run" || r.Fields["attributes.name"] != "collide" {
		t.Errorf("name=%v attributes.name=%v", r.Fields["name"], r.Fields["attributes.name"])
	}
	line := `{"span_id":"s1","start":"2026-11-17T17:01:00Z","service.name":"svc","attributes":{"service.name":"x","timestamp":"t","severity":"s"}}`
	r, ok := NormalizeSpan([]byte(line))
	if !ok {
		t.Fatal("not a span")
	}
	f := r.Fields
	if f["service.name"] != "svc" || f["timestamp"] != "2026-11-17T17:01:00Z" || f["severity"] != "INFO" {
		t.Errorf("top-level overwritten: %v", f)
	}
	if f["attributes.service.name"] != "x" || f["attributes.timestamp"] != "t" || f["attributes.severity"] != "s" {
		t.Errorf("attributes not kept: %v", f)
	}
}

func TestNormalizeSpanMapAttributeCollision(t *testing.T) {
	line := `{"span_id":"s1","start":"2026-11-17T17:01:00Z","service.name":"svc","otel":{"resource":"r"},"attributes":{"service":{"name":"x"},"otel":{"resource":{"a":"b"}},"ok":{"n":1}}}`
	r, ok := NormalizeSpan([]byte(line))
	if !ok {
		t.Fatal("not a span")
	}
	f := r.Fields
	if f["service.name"] != "svc" || f["otel.resource"] != "r" {
		t.Errorf("top-level overwritten: %v", f)
	}
	if f["attributes.service.name"] != "x" || f["attributes.otel.resource.a"] != "b" || f["ok.n"] == nil {
		t.Errorf("attributes not kept: %v", f)
	}
}

func TestNormalizeRoutesSpans(t *testing.T) {
	in := config.Default().Input
	line := spanLines(t)[3]
	want, _ := NormalizeSpan(line)
	got, ok := Normalize(adapter.New(in).ParseLine(line), in)
	if !ok || got.Stream != want.Stream || got.Source != want.Source || !reflect.DeepEqual(got.Fields, want.Fields) {
		t.Errorf("Normalize = %+v ok=%v, want %+v", got, ok, want)
	}
}

func TestNormalizeLogLineWithSpanIDStaysLog(t *testing.T) {
	in := config.Default().Input
	line := []byte(`{"time":"2026-11-17T17:00:00Z","trace_id":"t","span_id":"s","labels":{"schema":"api"},"message":"m"}`)
	r, ok := Normalize(adapter.New(in).ParseLine(line), in)
	if !ok || r.Stream != "api" || r.Source != SourceLog {
		t.Errorf("got %q %q ok=%v", r.Stream, r.Source, ok)
	}
	line = []byte(`{"time":"2026-11-17T17:00:00Z","trace_id":"t","span_id":"s","message":"m"}`)
	if _, ok := Normalize(adapter.New(in).ParseLine(line), in); ok {
		t.Error("log line without a stream was kept")
	}
}

func TestNormalizeSpanShapeWithLabelsSchemaIsLog(t *testing.T) {
	in := config.Default().Input
	for _, extra := range []string{
		`"logging.googleapis.com/labels":{"schema":"db"}`,
		`"labels":{"schema":"db"}`,
	} {
		line := []byte(`{"span_id":"s","start":"2026-11-17T17:00:00Z","time":"2026-11-17T17:00:00Z",` + extra + `}`)
		r, ok := Normalize(adapter.New(in).ParseLine(line), in)
		if !ok || r.Source != SourceLog || r.Stream != "db" {
			t.Errorf("%s: got %q %q ok=%v", extra, r.Stream, r.Source, ok)
		}
		if _, ok := NormalizeSpan(line); ok {
			t.Errorf("%s: NormalizeSpan accepted a log line", extra)
		}
	}
	for _, bad := range []string{`not json`, `[]`, `{"span_id":1,"start":"x"}`} {
		if _, ok := NormalizeSpan([]byte(bad)); ok {
			t.Errorf("NormalizeSpan(%q) = ok", bad)
		}
	}
}

func TestRecordKeyAndParseSource(t *testing.T) {
	if k := (Record{Stream: "db"}).Key(); k != (StreamKey{SourceLog, "db"}) {
		t.Errorf("Key = %v", k)
	}
	if k := (Record{Stream: "db", Source: SourceOtel}).Key(); k != (StreamKey{SourceOtel, "db"}) {
		t.Errorf("Key = %v", k)
	}
	for in, want := range map[string]string{"": SourceLog, "log": SourceLog, "otel": SourceOtel} {
		if got, ok := ParseSource(in); !ok || got != want {
			t.Errorf("ParseSource(%q) = %q,%v", in, got, ok)
		}
	}
	if got, ok := ParseSource("x"); ok || got != "" {
		t.Errorf("ParseSource(x) = %q,%v", got, ok)
	}
}
