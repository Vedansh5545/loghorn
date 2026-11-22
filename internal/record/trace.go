package record

import "strings"

// NormalizeTraceID returns the bare lower-case trace ID of s: surrounding
// space trimmed and anything up to the last "/traces/" dropped (the Cloud
// Trace form projects/<project>/traces/<id>). It returns "" for an empty or
// all-zero ID, which counts as no trace.
func NormalizeTraceID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.LastIndex(s, "/traces/"); i >= 0 {
		s = s[i+len("/traces/"):]
	}
	return nonZero(s)
}

// NormalizeSpanID returns s trimmed and lower-cased, or "" for an empty or
// all-zero ID.
func NormalizeSpanID(s string) string {
	return nonZero(strings.ToLower(strings.TrimSpace(s)))
}

func nonZero(s string) string {
	if strings.Trim(s, "0") == "" {
		return ""
	}
	return s
}

// TraceRef returns the normalised trace and span IDs a record's fields carry:
// trace_id else trace, span_id else spanId. A value that is not a string is
// ignored.
func TraceRef(fields map[string]any) (traceID, spanID string) {
	for _, k := range []string{"trace_id", "trace"} {
		if v, ok := fields[k].(string); ok {
			if traceID = NormalizeTraceID(v); traceID != "" {
				break
			}
		}
	}
	for _, k := range []string{"span_id", "spanId"} {
		if v, ok := fields[k].(string); ok {
			if spanID = NormalizeSpanID(v); spanID != "" {
				break
			}
		}
	}
	return traceID, spanID
}
