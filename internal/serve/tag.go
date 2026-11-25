package serve

import (
	"bytes"
	"encoding/json"

	"github.com/jimbeveridge/loghorn/internal/ingest"
)

// taggedSink wraps a sink so a run's JSON records name their service and run
// in loghorn.log, not only in memory: serve's preload after a restart reads
// the file with no other way to know which run wrote a line. A record that
// already names a service keeps it; non-JSON lines are written as they are.
func taggedSink(sink func([]byte), service, runID string) func([]byte) {
	if service == "" && runID == "" {
		return sink
	}
	return func(rec []byte) { sink(tagRecord(rec, service, runID)) }
}

// serviceKeys are the top-level keys a record may already name its service
// with (see record.ServiceKey); "resource" holds Cloud Logging's labels.
var serviceKeys = []string{"service.name", "service.namespace", "otel.resource.service.namespace", "resource"}

func tagRecord(rec []byte, service, runID string) []byte {
	c := bytes.TrimSpace(ingest.CompactRecord(rec))
	if len(c) < 2 || c[0] != '{' {
		return rec
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(c, &m) != nil {
		return rec
	}
	var add [][]byte
	named := false
	for _, k := range serviceKeys {
		if _, ok := m[k]; ok {
			named = true
		}
	}
	if service != "" && !named {
		add = append(add, field("service.name", service))
	}
	if _, ok := m["loghorn.run.id"]; runID != "" && !ok {
		add = append(add, field("loghorn.run.id", runID))
	}
	if len(add) == 0 {
		return rec
	}
	out := []byte{'{'}
	out = append(out, bytes.Join(add, []byte{','})...)
	if len(m) > 0 {
		out = append(out, ',')
	}
	return append(out, c[1:]...)
}

func field(k, v string) []byte {
	kb, _ := json.Marshal(k)
	vb, _ := json.Marshal(v)
	return append(append(kb, ':'), vb...)
}
