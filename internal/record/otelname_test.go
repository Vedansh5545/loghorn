package record

import "testing"

func TestOTelName(t *testing.T) {
	cases := []struct {
		source, path, want string
		ok                 bool
	}{
		{SourceOtel, "attributes.name", "name", true},           // collided with the span's name
		{SourceOtel, "attributes.span.type", "span.type", true}, // collided with span.type
		{SourceOtel, "http.method", "", false},                  // stored under its own key
		{SourceOtel, "attributes", "", false},                   // a non-object attributes value
		{SourceOtel, "duration_ms", "", false},                  // a span field
		{SourceOtel, "otel.attributes.message", "", false},      // log rule only
		{SourceLog, "otel.attributes.message", "message", true}, // OTLP log attribute moved aside
		{"", "otel.attributes.severity", "severity", true},      // "" is a log source
		{SourceLog, "http_request.request_method", "", false},   // snake_case LogEntry key: not OTel
		{SourceLog, "attributes.name", "", false},               // span rule only
		{SourceLog, "otel.attributes.", "", false},              // no key
	}
	for _, c := range cases {
		got, ok := OTelName(c.source, c.path)
		if got != c.want || ok != c.ok {
			t.Errorf("OTelName(%q, %q) = %q, %v; want %q, %v", c.source, c.path, got, ok, c.want, c.ok)
		}
	}
}
