package record

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

// Sources of a record: the log file's records, or spans from -otel.
const (
	SourceLog  = "log"
	SourceOtel = "otel"
)

// Span stream names.
const (
	StreamGenAI      = "gen_ai"
	StreamMessaging  = "messaging"
	StreamRPC        = "rpc"
	StreamDB         = "db"
	StreamHTTPServer = "http.server"
	StreamHTTPClient = "http.client"
	StreamSpan       = "span"
)

// StreamKey identifies a stream: the same name under two sources is two streams.
type StreamKey struct{ Source, Name string }

// Key returns the record's stream key; an empty Source means SourceLog.
func (r Record) Key() StreamKey {
	s := r.Source
	if s == "" {
		s = SourceLog
	}
	return StreamKey{Source: s, Name: r.Stream}
}

// ParseSource validates a source value; "" means SourceLog.
func ParseSource(s string) (string, bool) {
	switch s {
	case "", SourceLog:
		return SourceLog, true
	case SourceOtel:
		return SourceOtel, true
	}
	return "", false
}

func has(attrs map[string]any, keys ...string) bool {
	for _, k := range keys {
		if attrs[k] != nil {
			return true
		}
	}
	return false
}

var spanRules = []struct {
	stream string
	match  func(kind string, attrs map[string]any) bool
}{
	{StreamGenAI, func(_ string, a map[string]any) bool {
		for k, v := range a {
			if v != nil && strings.HasPrefix(k, "gen_ai.") {
				return true
			}
		}
		return false
	}},
	{StreamMessaging, func(_ string, a map[string]any) bool { return has(a, "messaging.system.name", "messaging.system") }},
	{StreamRPC, func(_ string, a map[string]any) bool { return has(a, "rpc.system.name", "rpc.system") }},
	{StreamDB, func(k string, a map[string]any) bool { return k == "client" && has(a, "db.system.name", "db.system") }},
	{StreamHTTPServer, func(k string, a map[string]any) bool {
		return k == "server" && has(a, "http.request.method", "http.method")
	}},
	{StreamHTTPClient, func(k string, a map[string]any) bool {
		return k == "client" && has(a, "http.request.method", "http.method")
	}},
}

// SpanStream names the stream of a span by its kind and attributes: the first
// matching rule wins, else StreamSpan. An attribute is present when its key
// exists with a non-nil value.
func SpanStream(kind string, attrs map[string]any) string {
	for _, r := range spanRules {
		if r.match(kind, attrs) {
			return r.stream
		}
	}
	return StreamSpan
}

// isSpan reports whether obj has span shape: string span_id and start, and no
// labels.schema in any spelling (such a line is a log line).
func isSpan(obj map[string]any) bool {
	if _, ok := obj["span_id"].(string); !ok {
		return false
	}
	if _, ok := obj["start"].(string); !ok {
		return false
	}
	if _, ok := obj["labels.schema"]; ok {
		return false
	}
	for _, k := range []string{"labels", "logging.googleapis.com/labels"} {
		if m, ok := obj[k].(map[string]any); ok {
			if _, ok := m["schema"]; ok {
				return false
			}
		}
	}
	return true
}

// NormalizeSpan builds the record for a span line. It returns false unless the
// line is a JSON object of span shape.
func NormalizeSpan(line []byte) (Record, bool) {
	raw := bytes.TrimSpace(line)
	if len(raw) == 0 || raw[0] != '{' {
		return Record{}, false
	}
	var obj map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return Record{}, false
	}
	return spanRecord(obj, raw)
}

// spanRecord converts an already decoded span-shaped object.
func spanRecord(obj map[string]any, raw []byte) (Record, bool) {
	if !isSpan(obj) {
		return Record{}, false
	}
	fields := map[string]any{}
	for _, k := range sortedKeys(obj) {
		if k != "attributes" {
			flatten(fields, k, obj[k])
		}
	}
	start, _ := obj["start"].(string)
	fields["timestamp"] = start
	if s, _ := obj["status"].(string); s == "error" {
		fields["severity"] = "ERROR"
	} else {
		fields["severity"] = "INFO"
	}
	attrs, isMap := obj["attributes"].(map[string]any)
	if !isMap {
		if v, ok := obj["attributes"]; ok {
			fields["attributes"] = v
		}
	}
	for _, k := range sortedKeys(attrs) {
		key := k
		if _, exists := fields[key]; exists {
			key = "attributes." + k
		}
		flatten(fields, key, attrs[k])
	}
	kind, _ := obj["kind"].(string)
	r := Record{Source: SourceOtel, Stream: SpanStream(kind, attrs), Fields: fields, Raw: append([]byte(nil), raw...)}
	if t, err := time.Parse(time.RFC3339Nano, start); err == nil {
		r.Time = t
	}
	return r, true
}
