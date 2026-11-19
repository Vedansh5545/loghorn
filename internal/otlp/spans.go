package otlp

import (
	"encoding/hex"
	"encoding/json"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// spanLine is one stored span. The field order is the order keys appear in
// the line; trace_id, span_id and name are always present.
type spanLine struct {
	TraceID       string         `json:"trace_id"`
	SpanID        string         `json:"span_id"`
	ParentSpanID  string         `json:"parent_span_id,omitempty"`
	Name          string         `json:"name"`
	Kind          string         `json:"kind,omitempty"`
	Start         string         `json:"start,omitempty"`
	End           string         `json:"end,omitempty"`
	DurationMS    *float64       `json:"duration_ms,omitempty"`
	Status        string         `json:"status,omitempty"`
	StatusMessage string         `json:"status_message,omitempty"`
	Attributes    map[string]any `json:"attributes,omitempty"`
	Events        []spanEvent    `json:"events,omitempty"`
	Links         []spanLink     `json:"links,omitempty"`
	ServiceName   any            `json:"service.name,omitempty"`
	RunID         any            `json:"loghorn.run.id,omitempty"`
	Resource      map[string]any `json:"otel.resource,omitempty"`
	ScopeName     string         `json:"otel.scope.name,omitempty"`
	ScopeVersion  string         `json:"otel.scope.version,omitempty"`
}

type spanEvent struct {
	Time       string         `json:"time,omitempty"`
	Name       string         `json:"name"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

type spanLink struct {
	TraceID    string         `json:"trace_id"`
	SpanID     string         `json:"span_id"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

var spanKinds = map[tracepb.Span_SpanKind]string{
	tracepb.Span_SPAN_KIND_INTERNAL: "internal",
	tracepb.Span_SPAN_KIND_SERVER:   "server",
	tracepb.Span_SPAN_KIND_CLIENT:   "client",
	tracepb.Span_SPAN_KIND_PRODUCER: "producer",
	tracepb.Span_SPAN_KIND_CONSUMER: "consumer",
}

// MapSpans returns one compact JSON object (no trailing newline) per Span, in
// request order. Trace state, flags, dropped counts and schema URLs are not
// stored.
func MapSpans(req *coltracepb.ExportTraceServiceRequest) [][]byte {
	var out [][]byte
	for _, rs := range req.GetResourceSpans() {
		resAttrs := rs.GetResource().GetAttributes()
		for _, ss := range rs.GetScopeSpans() {
			scope := ss.GetScope()
			for _, sp := range ss.GetSpans() {
				b, err := json.Marshal(mapSpan(sp, resAttrs, scope))
				if err != nil {
					continue // unreachable: every value is built from JSON-safe types
				}
				out = append(out, b)
			}
		}
	}
	return out
}

func mapSpan(sp *tracepb.Span, resAttrs []*commonpb.KeyValue, scope *commonpb.InstrumentationScope) spanLine {
	l := spanLine{
		TraceID:       hex.EncodeToString(sp.GetTraceId()),
		SpanID:        hex.EncodeToString(sp.GetSpanId()),
		Name:          sp.GetName(),
		Kind:          spanKinds[sp.GetKind()],
		Start:         unixNano(sp.GetStartTimeUnixNano()),
		End:           unixNano(sp.GetEndTimeUnixNano()),
		StatusMessage: sp.GetStatus().GetMessage(),
		Attributes:    attrMap(sp.GetAttributes()),
		ScopeName:     scope.GetName(),
		ScopeVersion:  scope.GetVersion(),
	}
	if id := sp.GetParentSpanId(); nonZero(id) {
		l.ParentSpanID = hex.EncodeToString(id)
	}
	if s, e := sp.GetStartTimeUnixNano(), sp.GetEndTimeUnixNano(); s != 0 && e != 0 && e >= s {
		d := float64(e-s) / 1e6
		l.DurationMS = &d
	}
	switch sp.GetStatus().GetCode() {
	case tracepb.Status_STATUS_CODE_OK:
		l.Status = "ok"
	case tracepb.Status_STATUS_CODE_ERROR:
		l.Status = "error"
	}
	for _, ev := range sp.GetEvents() {
		l.Events = append(l.Events, spanEvent{
			Time:       unixNano(ev.GetTimeUnixNano()),
			Name:       ev.GetName(),
			Attributes: attrMap(ev.GetAttributes()),
		})
	}
	for _, ln := range sp.GetLinks() {
		l.Links = append(l.Links, spanLink{
			TraceID:    hex.EncodeToString(ln.GetTraceId()),
			SpanID:     hex.EncodeToString(ln.GetSpanId()),
			Attributes: attrMap(ln.GetAttributes()),
		})
	}

	// Resource: the same split as for log records.
	other := map[string]any{}
	for _, kv := range resAttrs {
		switch kv.GetKey() {
		case "service.name":
			l.ServiceName = anyValue(kv.GetValue())
		case "loghorn.run.id":
			l.RunID = anyValue(kv.GetValue())
		default:
			other[kv.GetKey()] = anyValue(kv.GetValue())
		}
	}
	if len(other) > 0 {
		l.Resource = other
	}
	return l
}

// unixNano formats a non-zero OTLP timestamp as RFC 3339 UTC; zero is "".
func unixNano(n uint64) string {
	if n == 0 {
		return ""
	}
	return time.Unix(0, int64(n)).UTC().Format(time.RFC3339Nano)
}

// attrMap turns attributes into an object, keys verbatim; none is nil.
func attrMap(kvs []*commonpb.KeyValue) map[string]any {
	if len(kvs) == 0 {
		return nil
	}
	m := make(map[string]any, len(kvs))
	for _, kv := range kvs {
		m[kv.GetKey()] = anyValue(kv.GetValue())
	}
	return m
}
