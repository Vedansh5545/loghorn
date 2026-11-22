// Package trace builds the tree of one trace from the session's records: its
// spans and their events, the log lines that carry its trace ID, and a
// placeholder for each span that other records name but the session never saw.
package trace

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jimbeveridge/loghorn/internal/record"
)

// Limit is the most spans, the most logs, and the most span events one trace
// answer holds.
const Limit = 10000

// maxMessage is the longest log message a node carries, in runes.
const maxMessage = 500

// Node kinds.
const (
	KindSpan    = "span"
	KindLog     = "log"
	KindMissing = "missing"
	KindEvent   = "event"
)

// Trace is the answer for one trace: nodes in pre-order.
type Trace struct {
	TraceID    string      `json:"traceId"`
	Start      time.Time   `json:"start"`
	DurationMs float64     `json:"durationMs"`
	SpanCount  int         `json:"spanCount"`
	LogCount   int         `json:"logCount"`
	EventCount int         `json:"eventCount"`
	Services   []string    `json:"services"` // sorted, non-empty strings only
	Truncated  bool        `json:"truncated"`
	Nodes      []Node      `json:"nodes"`      // pre-order
	SerialRuns []SerialRun `json:"serialRuns"` // set by analyze
	SelfTime   []NameTime  `json:"selfTime"`   // set by analyze
}

// Node is a span, a span event, a log line or a placeholder for an absent span.
type Node struct {
	ID         string         `json:"id"`     // span ID, "missing:<id>", "log:<seq>", "event:<span id>:<index>"
	Parent     string         `json:"parent"` // "" at top level
	Depth      int            `json:"depth"`
	Kind       string         `json:"kind"`                 // "span" | "log" | "missing" | "event"
	Seq        int            `json:"seq,omitempty"`        // not set for events
	Name       string         `json:"name,omitempty"`       // span or event name; missing: the absent ID
	Type       string         `json:"type,omitempty"`       // span.type
	Service    string         `json:"service,omitempty"`    //
	StartMs    float64        `json:"startMs"`              // from Trace.Start
	DurationMs float64        `json:"durationMs"`           // 0 for logs and events
	SelfMs     float64        `json:"selfMs"`               // set by analyze
	Error      bool           `json:"error,omitempty"`      // span status "error"; an "exception" event
	Critical   bool           `json:"critical,omitempty"`   // set by analyze
	Children   int            `json:"children"`             // direct children, spans + logs + events
	LogTicks   []float64      `json:"logTicks,omitempty"`   // StartMs of direct child logs
	EventTicks []EventTick    `json:"eventTicks,omitempty"` // direct child events
	Severity   string         `json:"severity,omitempty"`   // logs
	Message    string         `json:"message,omitempty"`    // logs: message, else msg, else raw; exception events: "type: message"
	Time       string         `json:"time,omitempty"`       // events: the time as stored
	Attributes map[string]any `json:"attributes,omitempty"` // events
}

// EventTick marks a child event on its span's bar.
type EventTick struct {
	Ms    float64 `json:"ms"`
	Error bool    `json:"error,omitempty"`
}

// SerialRun is a run of sibling spans that follow one another.
type SerialRun struct {
	Parent string   `json:"parent"`
	First  string   `json:"first"`
	Spans  []string `json:"spans"`
	Count  int      `json:"count"`
	WallMs float64  `json:"wallMs"`
}

// NameTime is the time spent in the spans of one name.
type NameTime struct {
	Name    string  `json:"name"`
	Count   int     `json:"count"`
	TotalMs float64 `json:"totalMs"`
	SelfMs  float64 `json:"selfMs"`
}

// Find returns the trace ID of the span record with spanID, else of the first
// log record with that span ID and a trace ID, else "".
func Find(recs []record.Record, spanID string) string {
	spanID = record.NormalizeSpanID(spanID)
	if spanID == "" {
		return ""
	}
	logTrace := ""
	for i := range recs {
		r := &recs[i]
		tid, sid := record.TraceRef(r.Fields)
		if sid != spanID || tid == "" {
			continue
		}
		if r.Source == record.SourceOtel {
			return tid
		}
		if logTrace == "" {
			logTrace = tid
		}
	}
	return logTrace
}

// item is a node under construction.
type item struct {
	Node
	start, end time.Time // real times; zero for a placeholder until extents
	kids       []*item
	placed     bool
}

// Build returns the tree of traceID, or false when no record carries it.
// limit applies separately to spans, logs and span events, lowest Seq first.
func Build(recs []record.Record, traceID string, limit int) (Trace, bool) {
	traceID = record.NormalizeTraceID(traceID)
	if traceID == "" {
		return Trace{}, false
	}
	var spanRecs, logRecs []*record.Record
	for i := range recs {
		r := &recs[i]
		if tid, _ := record.TraceRef(r.Fields); tid != traceID {
			continue
		}
		if r.Source == record.SourceOtel {
			spanRecs = append(spanRecs, r)
		} else {
			logRecs = append(logRecs, r)
		}
	}
	if len(spanRecs) == 0 && len(logRecs) == 0 {
		return Trace{}, false
	}
	bySeq := func(rs []*record.Record) { sort.SliceStable(rs, func(i, j int) bool { return rs[i].Seq < rs[j].Seq }) }
	bySeq(spanRecs)
	bySeq(logRecs)

	out := Trace{TraceID: traceID, Services: []string{}, Nodes: []Node{}, SerialRuns: []SerialRun{}, SelfTime: []NameTime{}}

	// Spans: one per span ID (lowest Seq), then the limit.
	spans := map[string]*item{}
	var spanItems, eventItems []*item
	parentOf := map[string]string{}
	for _, r := range spanRecs {
		_, sid := record.TraceRef(r.Fields)
		if sid == "" || spans[sid] != nil {
			continue
		}
		if len(spanItems) >= limit {
			out.Truncated = true
			continue
		}
		it := spanItem(r, sid)
		spans[sid] = it
		spanItems = append(spanItems, it)
		p, _ := r.Fields["parent_span_id"].(string)
		parentOf[sid] = record.NormalizeSpanID(p)
		// Its events are its children, under their own limit.
		for _, ev := range eventItemsOf(it, r.Fields["events"]) {
			if len(eventItems) >= limit {
				out.Truncated = true
				break
			}
			eventItems = append(eventItems, ev)
			it.kids = append(it.kids, ev)
		}
	}
	var logItems []*item
	logSpan := map[*item]string{}
	for _, r := range logRecs {
		if len(logItems) >= limit {
			out.Truncated = true
			break
		}
		it := logItem(r)
		_, sid := record.TraceRef(r.Fields)
		logItems = append(logItems, it)
		logSpan[it] = sid
	}
	out.SpanCount, out.LogCount, out.EventCount = len(spanItems), len(logItems), len(eventItems)

	// Trace start: the earliest real time.
	for _, it := range append(append(append([]*item{}, spanItems...), logItems...), eventItems...) {
		if !it.start.IsZero() && (out.Start.IsZero() || it.start.Before(out.Start)) {
			out.Start = it.start
		}
	}

	// Link.
	missing := map[string]*item{}
	placeholder := func(id string) *item {
		if p := missing[id]; p != nil {
			return p
		}
		p := &item{Node: Node{ID: "missing:" + id, Kind: KindMissing, Name: id}}
		missing[id] = p
		return p
	}
	var tops []*item
	var rootSpans []*item
	for _, it := range spanItems {
		p := parentOf[it.ID]
		switch {
		case p == "" || p == it.ID:
			rootSpans = append(rootSpans, it)
			tops = append(tops, it)
		case spans[p] != nil:
			spans[p].kids = append(spans[p].kids, it)
		default:
			ph := placeholder(p)
			ph.kids = append(ph.kids, it)
		}
	}
	for _, it := range logItems {
		sid := logSpan[it]
		switch {
		case sid != "" && spans[sid] != nil:
			spans[sid].kids = append(spans[sid].kids, it)
		case sid != "":
			ph := placeholder(sid)
			ph.kids = append(ph.kids, it)
		case len(rootSpans) == 1:
			rootSpans[0].kids = append(rootSpans[0].kids, it)
		default:
			tops = append(tops, it)
		}
	}
	var phs []*item
	for _, ph := range missing {
		phs = append(phs, ph)
	}
	sort.Slice(phs, func(i, j int) bool { return phs[i].ID < phs[j].ID })
	for _, ph := range phs {
		extents(ph)
		tops = append(tops, ph)
	}

	// Offsets from Start.
	ms := func(t time.Time) float64 {
		if t.IsZero() || t.Before(out.Start) {
			return 0
		}
		return finite(float64(t.Sub(out.Start)) / float64(time.Millisecond))
	}
	var traceEnd time.Time
	all := append(append(append(append([]*item{}, spanItems...), logItems...), eventItems...), phs...)
	for _, it := range all {
		it.StartMs = ms(it.start)
		if !isLeaf(it.Kind) {
			it.DurationMs = math.Max(ms(it.end)-it.StartMs, 0)
		}
		if it.end.After(traceEnd) {
			traceEnd = it.end
		}
		if it.Kind != KindMissing && it.Service != "" {
			out.Services = append(out.Services, it.Service)
		}
	}
	out.DurationMs = ms(traceEnd)
	sort.Strings(out.Services)
	out.Services = dedupe(out.Services)

	// Order and flatten.
	order := func(xs []*item) {
		sort.SliceStable(xs, func(i, j int) bool {
			a, b := xs[i], xs[j]
			if !a.start.Equal(b.start) {
				return a.start.Before(b.start)
			}
			if isLeaf(a.Kind) != isLeaf(b.Kind) {
				return isLeaf(b.Kind)
			}
			return a.Seq < b.Seq
		})
	}
	for _, it := range all {
		order(it.kids)
	}
	order(tops)
	var walk func(it *item, parent string, depth int)
	walk = func(it *item, parent string, depth int) {
		it.placed = true
		idx := len(out.Nodes)
		n := it.Node
		n.Parent, n.Depth = parent, depth
		out.Nodes = append(out.Nodes, n)
		for _, k := range it.kids {
			if k.placed {
				continue
			}
			out.Nodes[idx].Children++
			switch k.Kind {
			case KindLog:
				out.Nodes[idx].LogTicks = append(out.Nodes[idx].LogTicks, k.StartMs)
			case KindEvent:
				out.Nodes[idx].EventTicks = append(out.Nodes[idx].EventTicks, EventTick{Ms: k.StartMs, Error: k.Error})
			}
			walk(k, it.ID, depth+1)
		}
	}
	for _, it := range tops {
		walk(it, "", 0)
	}
	// Spans in a parent cycle are reachable from no top-level node: start one
	// from each, earliest first.
	var rest []*item
	for _, it := range spanItems {
		if !it.placed {
			rest = append(rest, it)
		}
	}
	order(rest)
	for _, it := range rest {
		if !it.placed {
			walk(it, "", 0)
		}
	}
	analyze(&out)
	return out, true
}

// extents gives a placeholder the span of its descendants' times.
func extents(ph *item) {
	seen := map[*item]bool{ph: true}
	stack := append([]*item(nil), ph.kids...)
	for len(stack) > 0 {
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[it] {
			continue
		}
		seen[it] = true
		if !it.start.IsZero() && (ph.start.IsZero() || it.start.Before(ph.start)) {
			ph.start = it.start
		}
		if e := it.end; !e.IsZero() && e.After(ph.end) {
			ph.end = e
		}
		stack = append(stack, it.kids...)
	}
	if ph.end.Before(ph.start) {
		ph.end = ph.start
	}
}

func spanItem(r *record.Record, sid string) *item {
	f := r.Fields
	it := &item{Node: Node{ID: sid, Kind: KindSpan, Seq: r.Seq}}
	it.Name, _ = f["name"].(string)
	it.Type, _ = f["span.type"].(string)
	it.Service = service(r)
	s, _ := f["status"].(string)
	it.Error = s == "error"
	it.start = r.Time
	it.end = r.Time
	if s, _ := f["end"].(string); s != "" {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			it.end = t
		}
	} else if d, ok := number(f["duration_ms"]); ok && d > 0 {
		it.end = r.Time.Add(time.Duration(d * float64(time.Millisecond)))
	}
	if it.end.Before(it.start) {
		it.end = it.start
	}
	return it
}

// eventItemsOf returns the nodes of a span's events field (a JSON array of
// {time, name, attributes} objects), in array order. An entry that is not an
// object is skipped; one with no valid time sits at the span's start.
func eventItemsOf(sp *item, v any) []*item {
	evs, _ := v.([]any)
	var out []*item
	for i, e := range evs {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		it := &item{Node: Node{ID: "event:" + sp.ID + ":" + strconv.Itoa(i), Kind: KindEvent, Service: sp.Service}}
		it.Name, _ = m["name"].(string)
		it.Attributes, _ = m["attributes"].(map[string]any)
		it.start = sp.start
		if s, _ := m["time"].(string); s != "" {
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				it.start, it.Time = t, s
			}
		}
		it.end = it.start
		if it.Name == "exception" {
			it.Error = true
			var parts []string
			for _, k := range []string{"exception.type", "exception.message"} {
				if s, _ := it.Attributes[k].(string); s != "" {
					parts = append(parts, s)
				}
			}
			it.Message = truncate(strings.Join(parts, ": "))
		}
		out = append(out, it)
	}
	return out
}

// isLeaf reports whether a node kind is a point in time with no children.
func isLeaf(kind string) bool { return kind == KindLog || kind == KindEvent }

// truncate cuts s to maxMessage runes.
func truncate(s string) string {
	if rs := []rune(s); len(rs) > maxMessage {
		return string(rs[:maxMessage])
	}
	return s
}

func logItem(r *record.Record) *item {
	f := r.Fields
	it := &item{Node: Node{ID: "log:" + strconv.Itoa(r.Seq), Kind: KindLog, Seq: r.Seq}}
	it.Service = service(r)
	it.Severity, _ = f["severity"].(string)
	msg := ""
	for _, k := range []string{"message", "msg"} {
		if s, ok := f[k].(string); ok && s != "" {
			msg = s
			break
		}
	}
	if msg == "" {
		msg = strings.TrimSpace(string(r.Raw))
	}
	it.Message = truncate(msg)
	it.start, it.end = r.Time, r.Time
	return it
}

func service(r *record.Record) string {
	if s := record.ServiceKey(r.Fields); s != "" {
		return s
	}
	return r.Service
}

// number reads a JSON number field; NaN and Inf count as absent.
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
	case float64:
		return n, !math.IsNaN(n) && !math.IsInf(n, 0)
	}
	return 0, false
}

// finite maps NaN and Inf, which JSON cannot carry, to 0.
func finite(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return f
}

func dedupe(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}
