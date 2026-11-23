package record

import "strings"

// OTelName returns the original OpenTelemetry attribute key of a field when
// the field came from OTLP and loghorn stores it under a different path:
//
//   - a span attribute that collided with a span field is stored under
//     "attributes." (see spanRecord); the key is the path without it.
//   - an OTLP log attribute that collided with a mapped or reserved key is
//     stored under "otel.attributes." (see otlp.MapLogs); the key is the
//     path without it.
//
// Any other path is either the attribute key itself or not an attribute, and
// it reports false. LogEntry keys spelled in snake_case are not OTel
// attributes and are not reported.
func OTelName(source, path string) (string, bool) {
	prefix := "otel.attributes."
	if source == SourceOtel {
		prefix = "attributes."
	}
	if k, ok := strings.CutPrefix(path, prefix); ok && k != "" {
		return k, true
	}
	return "", false
}
