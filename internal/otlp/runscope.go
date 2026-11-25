package otlp

import (
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

// RunScope names the instrumentation scope of the log records loghorn run
// sends: each record's body is one framed input line, exactly as run would
// have written it to a log file. serve records them through a recorder with
// the run's input settings rather than mapping them as OTLP.
const RunScope = "loghorn.run"

// Resource attributes of a run's records.
const (
	AttrService = "service.name"
	AttrRunID   = "loghorn.run.id"
	AttrInput   = "loghorn.input" // the run's [input] settings as JSON
)

// RunLine is one line a loghorn run sent.
type RunLine struct {
	Service, RunID string
	Input          []byte // JSON of config.Input; empty when absent
	Line           []byte
}

// SplitRunLines removes every RunScope ScopeLogs from req and returns their
// records with a string body as RunLines, in request order. Records with any
// other body are dropped: run only sends strings.
func SplitRunLines(req *collogspb.ExportLogsServiceRequest) []RunLine {
	var out []RunLine
	for _, rl := range req.GetResourceLogs() {
		var keep []*logspb.ScopeLogs
		var svc, id string
		var input []byte
		attrsRead := false
		for _, sl := range rl.GetScopeLogs() {
			if sl.GetScope().GetName() != RunScope {
				keep = append(keep, sl)
				continue
			}
			if !attrsRead {
				attrsRead = true
				for _, kv := range rl.GetResource().GetAttributes() {
					v := kv.GetValue().GetStringValue()
					switch kv.GetKey() {
					case AttrService:
						svc = v
					case AttrRunID:
						id = v
					case AttrInput:
						input = []byte(v)
					}
				}
			}
			for _, lr := range sl.GetLogRecords() {
				s, ok := lr.GetBody().GetValue().(*commonpb.AnyValue_StringValue)
				if !ok {
					continue
				}
				out = append(out, RunLine{Service: svc, RunID: id, Input: input, Line: []byte(s.StringValue)})
			}
		}
		rl.ScopeLogs = keep
	}
	return out
}
