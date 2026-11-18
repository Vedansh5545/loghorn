// Package record turns an entry.Entry into the canonical record the web query
// view stores, infers schemas over and queries: one field set whether the line
// came from pino locally or from Cloud Logging.
package record

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/entry"
	"gopkg.in/yaml.v3"
)

// Record is one labelled log record in canonical form.
type Record struct {
	Seq           int            // assigned by store.Add, 1-based
	Time          time.Time      // effective time (store fills inherited ones)
	TimeInherited bool           // Time was not the record's own
	Stream        string         // labels.schema
	Fields        map[string]any // canonical dotted path -> string | json.Number | bool | nil | []any
	Raw           []byte         // the line as searched/shown; GitHub prefix stripped
}

// HTTPRequestStream is the stream of a LogEntry that has an httpRequest object
// but no labels.schema, such as a Cloud Run request log
// (run.googleapis.com/requests), which carries no payload at all. Without it
// those records would be skipped and their httpRequest.* fields never get a
// schema.
const HTTPRequestStream = "http-request"

// Normalize builds the canonical record for e. It returns false when the
// record has no JSON object, or has neither a labels.schema stream label nor
// an httpRequest object (see HTTPRequestStream).
func Normalize(e entry.Entry, in config.Input) (Record, bool) {
	raw := bytes.TrimSpace(e.Raw)

	var obj map[string]any
	switch {
	case len(raw) > 0 && raw[0] == '{':
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&obj); err != nil || obj == nil {
			return Record{}, false
		}
	case e.JSON != nil:
		// The adapter's map holds YAML integers as float64, which loses
		// 64-bit IDs; decode the document again so ints stay exact.
		var y map[string]any
		if yaml.Unmarshal(raw, &y) == nil && y != nil {
			obj = numberize(y).(map[string]any)
		} else {
			obj = numberize(e.JSON).(map[string]any)
		}
	default:
		return Record{}, false
	}

	sev := e.Severity

	n := normalizer{obj: obj, naming: in.FieldNaming, fields: map[string]any{}, used: map[string]bool{}, top: map[string]bool{}}
	n.promote(sev)
	n.payload()

	stream, _ := n.fields["labels.schema"].(string)
	if stream == "" && n.httpRequest {
		stream = HTTPRequestStream
	}
	if stream == "" {
		return Record{}, false
	}
	r := Record{Stream: stream, Fields: n.fields, Raw: append([]byte(nil), raw...)}
	if ts, ok := n.fields["timestamp"].(string); ok {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			r.Time = t
		}
	}
	if r.Time.IsZero() {
		// adapter.ParseLine already put a GitHub Actions line prefix's time
		// here when the JSON had none.
		r.Time = e.Timestamp
	}
	return r, true
}

type normalizer struct {
	obj    map[string]any
	naming config.FieldNaming
	fields map[string]any
	used   map[string]bool // root keys consumed by promotion
	top    map[string]bool // first path segments written by promotion

	httpRequest bool // the LogEntry has an httpRequest object
}

// lookup finds the LogEntry key camel under the configured spelling, as
// adapter.field does. It returns the key actually present.
func (n *normalizer) lookup(camel string) (key string, v any, ok bool) {
	if n.naming != config.NamingSnake {
		if v, ok := n.obj[camel]; ok {
			return camel, v, true
		}
		if n.naming == config.NamingCamel {
			return "", nil, false
		}
	}
	k := snakeCase(camel)
	v, ok = n.obj[k]
	return k, v, ok
}

func snakeCase(camel string) string {
	var b strings.Builder
	for i := 0; i < len(camel); i++ {
		if c := camel[i]; c >= 'A' && c <= 'Z' {
			b.WriteByte('_')
			b.WriteByte(c + 'a' - 'A')
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// set stores v under the canonical name, flattening maps.
func (n *normalizer) set(name string, v any) {
	n.top[strings.SplitN(name, ".", 2)[0]] = true
	flatten(n.fields, name, v)
}

func (n *normalizer) promote(sev entry.Severity) {
	// timestamp <- timestamp | time
	for _, k := range []string{"timestamp", "time"} {
		if v, ok := n.obj[k]; ok {
			n.used[k] = true
			n.set("timestamp", v)
			break
		}
	}
	if _, ok := n.obj["severity"]; ok {
		n.used["severity"] = true
		n.set("severity", sev.String())
	}
	// labels <- labels | logging.googleapis.com/labels
	for _, k := range []string{"labels", "logging.googleapis.com/labels"} {
		if v, ok := n.obj[k]; ok {
			n.used[k] = true
			n.set("labels", v)
		}
	}
	for _, name := range []string{"trace", "spanId"} {
		if v, ok := n.obj["logging.googleapis.com/"+name]; ok {
			n.used["logging.googleapis.com/"+name] = true
			n.set(name, v)
		}
	}
	for _, name := range []string{"trace", "spanId", "httpRequest", "resource", "receiveTimestamp",
		"insertId", "logName", "sourceLocation", "operation", "traceSampled"} {
		if k, v, ok := n.lookup(name); ok {
			n.used[k] = true
			n.set(name, v)
			if name == "httpRequest" {
				_, n.httpRequest = v.(map[string]any)
			}
		}
	}
	if k, v, ok := n.lookup("textPayload"); ok {
		n.used[k] = true
		n.set("message", v)
	}
}

// payload flattens jsonPayload and every unconsumed root key. A payload path
// that starts with a promoted name keeps a jsonPayload. prefix so it cannot
// overwrite the promoted value.
func (n *normalizer) payload() {
	add := func(path string, v any) {
		if n.top[strings.SplitN(path, ".", 2)[0]] {
			path = "jsonPayload." + path
		}
		if _, exists := n.fields[path]; exists {
			return
		}
		flatten(n.fields, path, v)
	}
	if k, v, ok := n.lookup("jsonPayload"); ok {
		if m, isMap := v.(map[string]any); isMap {
			n.used[k] = true
			for _, key := range sortedKeys(m) {
				add(key, m[key])
			}
		}
	}
	for _, key := range sortedKeys(n.obj) {
		if !n.used[key] {
			// Root keys cannot collide with promotion: promoted keys are consumed.
			if _, exists := n.fields[key]; !exists {
				flatten(n.fields, key, n.obj[key])
			}
		}
	}
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// flatten writes v at path, recursing into maps; arrays stay whole. An empty
// map is kept as a nil leaf so the field is not lost.
func flatten(out map[string]any, path string, v any) {
	m, ok := v.(map[string]any)
	if !ok {
		out[path] = v
		return
	}
	if len(m) == 0 {
		out[path] = nil
		return
	}
	for _, k := range sortedKeys(m) {
		flatten(out, path+"."+k, m[k])
	}
}

// numberize converts decoded YAML/JSON values to the canonical leaf types:
// float64 and the YAML integer types become json.Number.
func numberize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = numberize(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = numberize(x)
		}
		return out
	case float64:
		return json.Number(strconv.FormatFloat(t, 'f', -1, 64))
	case int:
		return json.Number(strconv.Itoa(t))
	case int64:
		return json.Number(strconv.FormatInt(t, 10))
	case uint64:
		return json.Number(strconv.FormatUint(t, 10))
	default:
		return v
	}
}
