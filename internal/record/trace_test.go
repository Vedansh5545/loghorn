package record

import (
	"encoding/json"
	"testing"
)

func TestTraceRef(t *testing.T) {
	const lower = "4bf92f3577b34da6a3ce929d0e0e4736"
	const upper = "4BF92F3577B34DA6A3CE929D0E0E4736"
	zero := "00000000000000000000000000000000"
	tests := []struct {
		name        string
		fields      map[string]any
		trace, span string
	}{
		{"upper case", map[string]any{"trace_id": upper}, lower, ""},
		{"cloud trace form", map[string]any{"trace": "projects/example-project/traces/" + lower}, lower, ""},
		{"cloud form upper", map[string]any{"trace": "projects/example-project/traces/" + upper}, lower, ""},
		{"all zero", map[string]any{"trace_id": zero}, "", ""},
		{"span_id", map[string]any{"span_id": "ABC"}, "", "abc"},
		{"spanId", map[string]any{"spanId": "abc"}, "", "abc"},
		{"zero span", map[string]any{"span_id": "0000000000000000"}, "", ""},
		{"number ignored", map[string]any{"trace_id": json.Number("5")}, "", ""},
		{"trace_id preferred", map[string]any{"trace_id": lower, "trace": "other"}, lower, ""},
		{"zero trace_id falls back", map[string]any{"trace_id": zero, "trace": "other"}, "other", ""},
		{"trimmed", map[string]any{"trace_id": "  " + upper + " "}, lower, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr, sp := TraceRef(tc.fields)
			if tr != tc.trace || sp != tc.span {
				t.Errorf("TraceRef = %q, %q; want %q, %q", tr, sp, tc.trace, tc.span)
			}
		})
	}
}
