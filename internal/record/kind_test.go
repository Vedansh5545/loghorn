package record

import "testing"

func spanOf(t *testing.T, line string) Record {
	t.Helper()
	r, ok := NormalizeSpan([]byte(line))
	if !ok {
		t.Fatalf("not a span: %s", line)
	}
	return r
}

func TestSpanKind(t *testing.T) {
	cases := []struct{ line, stream, typ string }{
		{`{"span_id":"a","start":"2026-11-17T17:00:00Z","kind":"client","attributes":{"db.system.name":"x"},"name":"SELECT t"}`, "db", "db"},
		{`{"span_id":"a","start":"2026-11-17T17:00:00Z","kind":"internal","name":"sweep orders"}`, "sweep orders", "span"},
	}
	for _, c := range cases {
		r := spanOf(t, c.line)
		if r.Stream != c.stream || r.Fields["span.type"] != c.typ {
			t.Errorf("%s: stream=%q span.type=%v", c.line, r.Stream, r.Fields["span.type"])
		}
	}
}

func TestKindEmptyName(t *testing.T) {
	r := spanOf(t, `{"span_id":"a","start":"2026-11-17T17:00:00Z","kind":"internal","name":""}`)
	if r.Stream != "span" {
		t.Errorf("stream = %q, want span", r.Stream)
	}
}

func TestSpanTypeAttributeCollision(t *testing.T) {
	r := spanOf(t, `{"span_id":"a","start":"2026-11-17T17:00:00Z","kind":"internal","name":"n","attributes":{"span.type":"mine"}}`)
	if r.Fields["span.type"] != "span" || r.Fields["attributes.span.type"] != "mine" {
		t.Errorf("fields = %v", r.Fields)
	}
}

func TestServiceKey(t *testing.T) {
	const pre = `{"span_id":"a","start":"2026-11-17T17:00:00Z","name":"n",`
	if got := spanOf(t, pre+`"service.name":"shop-api"}`).Service; got != "shop-api" {
		t.Errorf("service.name: %q", got)
	}
	if got := spanOf(t, pre+`"otel.resource.service.namespace":"mail","service.name":"mail-worker"}`).Service; got != "mail" {
		t.Errorf("namespace: %q", got)
	}
	cases := []struct{ name, line, want string }{
		{"otlp log", `{"service.name":"api","labels":{"schema":"db"},"msg":"x"}`, "api"},
		{"gcp", `{"labels":{"schema":"db"},"resource":{"labels":{"service_name":"web"}},"jsonPayload":{"a":1}}`, "web"},
		{"pino", `{"labels":{"schema":"db"},"level":30,"msg":"x"}`, ""},
	}
	for _, c := range cases {
		r, ok := norm(t, c.line, auto)
		if !ok || r.Service != c.want {
			t.Errorf("%s: ok=%v service=%q, want %q", c.name, ok, r.Service, c.want)
		}
	}
}

func TestIsSpanAttribute(t *testing.T) {
	for p, want := range map[string]bool{
		"db.query.text": true, "http.route": true, "attributes.name": true, "attributes.kind.x": true,
		"name": false, "kind": false, "span_id": false, "trace_id": false, "parent_span_id": false,
		"start": false, "end": false, "duration_ms": false, "status": false, "status_message": false,
		"events": false, "links": false, "service.name": false, "loghorn.run.id": false, "span.type": false, "span.event_count": false,
		"timestamp": false, "severity": false, "otel.resource.host.name": false, "otel.scope.name": false,
		"otel.scope.version": false, "attributes": false,
	} {
		if got := IsSpanAttribute(p); got != want {
			t.Errorf("IsSpanAttribute(%q)=%v want %v", p, got, want)
		}
	}
}
