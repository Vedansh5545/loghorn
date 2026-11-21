package schema

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jimbeveridge/loghorn/internal/record"
)

const maxDefaultColumns = 8

// kind is what one observed value looks like, before the path's values are
// merged into a single Type.
type kind int

const (
	kNone kind = iota // null or empty: says nothing about the type
	kInt
	kFloat
	kBool
	kJSON
	kDuration
	kTimestamp
	kString
)

func classify(v any) kind {
	switch x := v.(type) {
	case nil:
		return kNone
	case json.Number:
		if _, err := strconv.ParseInt(x.String(), 10, 64); err == nil {
			return kInt
		}
		return kFloat
	case bool:
		return kBool
	case []any:
		return kJSON
	case string:
		switch {
		case x == "":
			return kNone
		case isRFC3339(x):
			return kTimestamp
		case isUnitDuration(x):
			return kDuration
		}
		return kString
	}
	return kString
}

func isRFC3339(s string) bool {
	_, err := time.Parse(time.RFC3339, s)
	return err == nil
}

// isUnitDuration is a duration string that carries a unit; a bare "42" is
// just text for inference (a saved override can still make it a duration).
func isUnitDuration(s string) bool {
	if _, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
		return false
	}
	_, ok := ParseDuration(s)
	return ok
}

type acc struct {
	count int
	kinds map[kind]bool
}

func (a *acc) typ() Type {
	k := a.kinds
	delete(k, kNone)
	switch len(k) {
	case 0:
		return TString
	case 1:
		for only := range k {
			switch only {
			case kInt:
				return TInt
			case kFloat:
				return TFloat
			case kBool:
				return TBool
			case kJSON:
				return TJSON
			case kDuration:
				return TDuration
			case kTimestamp:
				return TTimestamp
			}
		}
	case 2:
		if k[kInt] && k[kFloat] {
			return TFloat
		}
	}
	return TString
}

// Infer builds a schema for the stream (source, stream) from its records;
// "" means the log source. service restricts it to one service's records;
// "" or "*" means every service. A source record.ParseSource refuses matches no
// records. It never reads a saved schema; merging is the caller's job.
func Infer(service, source, stream string, recs []record.Record) Schema {
	src, ok := record.ParseSource(source)
	if !ok {
		src = source
	}
	key := record.StreamKey{Source: src, Name: stream}
	accs := map[string]*acc{}
	for i := range recs {
		if !ok || recs[i].Key() != key {
			continue
		}
		if service != "" && service != "*" && recs[i].Service != service {
			continue
		}
		for path, v := range recs[i].Fields {
			a := accs[path]
			if a == nil {
				a = &acc{kinds: map[kind]bool{}}
				accs[path] = a
			}
			a.count++
			a.kinds[classify(v)] = true
		}
	}

	s := Schema{Stream: stream, Source: src, Fields: make([]Field, 0, len(accs))}
	for path, a := range accs {
		t := a.typ()
		if path == "timestamp" || path == "receiveTimestamp" {
			t = TTimestamp
		}
		s.Fields = append(s.Fields, Field{
			Path: path, Type: t, Inferred: t, Count: a.count,
			Visualizer: DefaultVisualizer(path, t),
		})
	}
	sort.Slice(s.Fields, func(i, j int) bool { return s.Fields[i].Path < s.Fields[j].Path })
	s.Columns = DefaultColumns(src, s.Fields)
	return s
}

// DefaultColumns picks the columns shown when a query names none: the
// source's lead fields that are present (timestamp, severity, message for
// log; timestamp, name, duration_ms for otel), then the rest by Count
// descending, capped at maxDefaultColumns. Hidden, labels.* and json fields
// are skipped.
func DefaultColumns(source string, fields []Field) []string {
	lead := []string{"timestamp", "severity", "message"}
	if src, _ := record.ParseSource(source); src == record.SourceOtel {
		lead = []string{"timestamp", "name", "duration_ms"}
	}
	byPath := map[string]Field{}
	for _, f := range fields {
		byPath[f.Path] = f
	}
	var cols []string
	used := map[string]bool{}
	for _, p := range lead {
		if _, ok := byPath[p]; ok {
			cols = append(cols, p)
			used[p] = true
		}
	}
	rest := make([]Field, 0, len(fields))
	for _, f := range fields {
		if used[f.Path] || f.Hidden || strings.HasPrefix(f.Path, "labels.") || f.Type == TJSON {
			continue
		}
		rest = append(rest, f)
	}
	sort.SliceStable(rest, func(i, j int) bool {
		if rest[i].Count != rest[j].Count {
			return rest[i].Count > rest[j].Count
		}
		return rest[i].Path < rest[j].Path
	})
	for _, f := range rest {
		if len(cols) >= maxDefaultColumns {
			break
		}
		cols = append(cols, f.Path)
	}
	return cols
}

// DefaultVisualizer picks how an unconfigured field is shown.
func DefaultVisualizer(path string, t Type) string {
	switch {
	case path == "pid", path == "httpRequest.status":
		return VPlain
	case path == "insertId", path == "trace", path == "spanId", path == "hostname", strings.HasPrefix(path, "labels."):
		return VRaw
	case path == "timestamp", path == "receiveTimestamp":
		return VLocalTime
	case path == "severity":
		return VSeverity
	case path == "httpRequest.latency":
		return VDuration
	case path == "httpRequest.requestUrl":
		return VLink
	}
	switch t {
	case TInt, TFloat:
		return VNumber
	case TDuration:
		return VDuration
	case TTimestamp:
		return VLocalTime
	case TJSON:
		return VJSON
	}
	return VRaw
}
