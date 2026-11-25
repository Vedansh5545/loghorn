// Package otlp turns OpenTelemetry log records into loghorn's stored lines.
package otlp

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/jimbeveridge/loghorn/internal/entry"
	"github.com/jimbeveridge/loghorn/internal/record"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

// Source is the value of the loghorn.source field on every mapped record.
const Source = record.OTLPSource

// reserved are the keys the normaliser reinterprets (record.promote) or that
// would be hidden behind a promoted field. A body key or attribute with one of
// these names is stored under an otel.* prefix instead.
var reserved = map[string]bool{}

func init() {
	for _, k := range strings.Fields(`timestamp time severity message labels event.name trace_id span_id
		service.name loghorn.run.id loghorn.source trace spanId httpRequest http_request
		resource textPayload text_payload jsonPayload json_payload receiveTimestamp
		receive_timestamp insertId insert_id logName log_name sourceLocation source_location
		operation traceSampled trace_sampled logging.googleapis.com/labels
		logging.googleapis.com/trace logging.googleapis.com/spanId`) {
		reserved[k] = true
	}
}

func isReserved(k string) bool {
	return reserved[k] || strings.HasPrefix(k, "otel.") || strings.HasPrefix(k, "labels.")
}

// MapLogs returns one compact JSON object (no trailing newline) per LogRecord,
// in request order. now is used only when both timestamps are zero.
func MapLogs(req *collogspb.ExportLogsServiceRequest, now time.Time) [][]byte {
	var out [][]byte
	for _, rl := range req.GetResourceLogs() {
		resAttrs := rl.GetResource().GetAttributes()
		for _, sl := range rl.GetScopeLogs() {
			scope := sl.GetScope()
			for _, lr := range sl.GetLogRecords() {
				m := mapRecord(lr, resAttrs, scope, now)
				b, err := json.Marshal(m)
				if err != nil {
					continue // unreachable: every value is built from JSON-safe types
				}
				out = append(out, b)
			}
		}
	}
	return out
}

func mapRecord(lr *logspb.LogRecord, resAttrs []*commonpb.KeyValue, scope *commonpb.InstrumentationScope, now time.Time) map[string]any {
	m := map[string]any{"loghorn.source": Source}
	// The reader derives the record's stream from event.name, then
	// otel.scope.name (record.otlpStream); nothing here invents a label.
	name := EventName(lr)
	if name != "" {
		m["event.name"] = name
	}

	ts := lr.GetTimeUnixNano()
	if ts == 0 {
		ts = lr.GetObservedTimeUnixNano()
	}
	t := now
	if ts != 0 {
		t = time.Unix(0, int64(ts))
	}
	m["timestamp"] = t.UTC().Format(time.RFC3339Nano)

	if s, ok := severity(lr); ok {
		m["severity"] = s
	}
	if id := lr.GetTraceId(); nonZero(id) {
		m["trace_id"] = hex.EncodeToString(id)
	}
	if id := lr.GetSpanId(); nonZero(id) {
		m["span_id"] = hex.EncodeToString(id)
	}

	// Resource.
	other := map[string]any{}
	for _, kv := range resAttrs {
		switch kv.GetKey() {
		case "service.name", "loghorn.run.id":
			m[kv.GetKey()] = anyValue(kv.GetValue())
		default:
			other[kv.GetKey()] = anyValue(kv.GetValue())
		}
	}
	if len(other) > 0 {
		m["otel.resource"] = other
	}
	if scope.GetName() != "" {
		m["otel.scope.name"] = scope.GetName()
	}
	if scope.GetVersion() != "" {
		m["otel.scope.version"] = scope.GetVersion()
	}

	body(m, lr.GetBody())

	for _, kv := range lr.GetAttributes() {
		k := kv.GetKey()
		if k == "event.name" && lr.GetEventName() == "" && kv.GetValue().GetStringValue() == name {
			continue // already stored as event.name above
		}
		if _, taken := m[k]; taken || isReserved(k) {
			k = "otel.attributes." + k
		}
		m[k] = anyValue(kv.GetValue())
	}
	return m
}

func body(m map[string]any, b *commonpb.AnyValue) {
	switch v := b.GetValue().(type) {
	case nil:
	case *commonpb.AnyValue_StringValue:
		m["message"] = v.StringValue
	case *commonpb.AnyValue_KvlistValue:
		vals := v.KvlistValue.GetValues()
		hasMessage := false
		for _, kv := range vals {
			if kv.GetKey() == "message" {
				hasMessage = true
			}
		}
		for _, kv := range vals {
			k := kv.GetKey()
			switch {
			case k == "message", k == "msg" && !hasMessage:
				m["message"] = anyValue(kv.GetValue())
			case isReserved(k):
				m["otel.body."+k] = anyValue(kv.GetValue())
			default:
				m[k] = anyValue(kv.GetValue())
			}
		}
	default:
		m["otel.body"] = anyValue(b)
	}
}

func severity(lr *logspb.LogRecord) (string, bool) {
	text := lr.GetSeverityText()
	if s := entry.ParseSeverity(text); s != entry.SevDefault {
		return s.String(), true
	}
	if strings.EqualFold(strings.TrimSpace(text), "DEFAULT") {
		return entry.SevDefault.String(), true
	}
	switch n := int32(lr.GetSeverityNumber()); {
	case n >= 1 && n <= 8:
		return "DEBUG", true
	case n >= 9 && n <= 12:
		return "INFO", true
	case n >= 13 && n <= 16:
		return "WARNING", true
	case n >= 17 && n <= 20:
		return "ERROR", true
	case n >= 21 && n <= 24:
		return "CRITICAL", true
	}
	if text != "" {
		return text, true
	}
	return "", false
}

func nonZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return true
		}
	}
	return false
}

// EventName is the record's event name: the EventName field, else a non-empty
// string event.name attribute (the older semantic-convention spelling).
func EventName(lr *logspb.LogRecord) string {
	if n := lr.GetEventName(); n != "" {
		return n
	}
	for _, kv := range lr.GetAttributes() {
		if kv.GetKey() == "event.name" {
			if s := kv.GetValue().GetStringValue(); s != "" {
				return s
			}
		}
	}
	return ""
}

func anyValue(v *commonpb.AnyValue) any {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_BoolValue:
		return x.BoolValue
	case *commonpb.AnyValue_IntValue:
		return x.IntValue
	case *commonpb.AnyValue_DoubleValue:
		switch d := x.DoubleValue; {
		case math.IsNaN(d):
			return "NaN"
		case math.IsInf(d, 1):
			return "+Inf"
		case math.IsInf(d, -1):
			return "-Inf"
		default:
			return d
		}
	case *commonpb.AnyValue_ArrayValue:
		arr := make([]any, 0, len(x.ArrayValue.GetValues()))
		for _, e := range x.ArrayValue.GetValues() {
			arr = append(arr, anyValue(e))
		}
		return arr
	case *commonpb.AnyValue_KvlistValue:
		obj := make(map[string]any, len(x.KvlistValue.GetValues()))
		for _, kv := range x.KvlistValue.GetValues() {
			obj[kv.GetKey()] = anyValue(kv.GetValue())
		}
		return obj
	case *commonpb.AnyValue_BytesValue:
		return base64.StdEncoding.EncodeToString(x.BytesValue)
	}
	return nil
}
