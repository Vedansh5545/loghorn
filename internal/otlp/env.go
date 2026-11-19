package otlp

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"
)

// NewRunID names one loghorn run: the UTC start time and four random hex
// characters, e.g. 20261119T204012Z-3fa2.
func NewRunID(now time.Time) string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

// ChildEnv returns a copy of environ for a child whose OTLP logs and traces go
// to the receiver at endpoint (http://127.0.0.1:<port>). The logs and traces
// endpoint, protocol and exporter variables are always set, overriding the
// user's. Each variable replaces an
// existing one in place or is appended, so no key appears twice. A generic
// OTEL_EXPORTER_OTLP_ENDPOINT already set (and non-empty) is the user's and is
// kept; OTEL_RESOURCE_ATTRIBUTES gains loghorn.run.id.
func ChildEnv(environ []string, endpoint, runID string) []string {
	out := make([]string, 0, len(environ)+9)
	seen := map[string]bool{}
	value := map[string]string{}
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		// Later duplicates win, as in a process's environment.
		value[k] = v
	}
	set := map[string]string{
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT":   endpoint + "/v1/logs",
		"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL":   "http/protobuf",
		"OTEL_LOGS_EXPORTER":                 "otlp",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": endpoint + "/v1/traces",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "http/protobuf",
		"OTEL_TRACES_EXPORTER":               "otlp",
		"LOGHORN_RUN_ID":                     runID,
	}
	if value["OTEL_EXPORTER_OTLP_ENDPOINT"] == "" {
		set["OTEL_EXPORTER_OTLP_ENDPOINT"] = endpoint
	}
	attrs := "loghorn.run.id=" + runID
	if v := value["OTEL_RESOURCE_ATTRIBUTES"]; v != "" {
		attrs = v + "," + attrs
	}
	set["OTEL_RESOURCE_ATTRIBUTES"] = attrs
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if seen[k] {
			continue
		}
		seen[k] = true
		if v, ok := set[k]; ok {
			out = append(out, k+"="+v)
		} else {
			out = append(out, kv)
		}
	}
	for _, k := range []string{
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL", "OTEL_LOGS_EXPORTER",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_TRACES_EXPORTER",
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_RESOURCE_ATTRIBUTES", "LOGHORN_RUN_ID",
	} {
		if v, ok := set[k]; ok && !seen[k] {
			out = append(out, k+"="+v)
		}
	}
	return out
}
