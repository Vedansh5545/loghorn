package trace

import (
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/jimbeveridge/loghorn/internal/record"
)

// AggCap is the most traces one aggregate merges; the most recent are kept.
const AggCap = 1000

// AggOptions selects the traces Aggregate merges.
type AggOptions struct {
	Name    string
	Service string    // "" or "*": every service
	From    time.Time // zero: open
	To      time.Time // zero: open
	Cap     int       // <= 0 means AggCap
}

// Slowest names the longest call of a merged node.
type Slowest struct {
	TraceID    string  `json:"traceId"`
	SpanID     string  `json:"spanId"`
	DurationMs float64 `json:"durationMs"`
}

// OpNode is the merge of the spans at one path of span names below the
// operation root, over every aggregated trace.
type OpNode struct {
	ID             string   `json:"id"`     // "n<index>"
	Parent         string   `json:"parent"` // "" for the root
	Depth          int      `json:"depth"`
	Children       int      `json:"children"`
	Name           string   `json:"name"`
	Type           string   `json:"type"`
	Services       []string `json:"services"`
	Traces         int      `json:"traces"`
	Calls          int      `json:"calls"`
	CallsPerTrace  float64  `json:"callsPerTrace"`
	P50Ms          float64  `json:"p50Ms"`
	P95Ms          float64  `json:"p95Ms"`
	TotalMs        float64  `json:"totalMs"`
	SelfMs         float64  `json:"selfMs"`
	TotalPct       float64  `json:"totalPct"`
	SelfPct        float64  `json:"selfPct"`
	Errors         int      `json:"errors"`
	SerialCalls    int      `json:"serialCalls"`
	SerialMs       float64  `json:"serialMs"`
	SerialPerTrace float64  `json:"serialPerTrace"`
	SerialPct      float64  `json:"serialPct"`
	Slowest        Slowest  `json:"slowest"`
}

// Operation is the aggregate of the traces whose operation root is named Name.
type Operation struct {
	Name             string    `json:"name"`
	Service          string    `json:"service"`
	From             time.Time `json:"from,omitzero"`
	To               time.Time `json:"to,omitzero"`
	Traces           int       `json:"traces"`
	Matched          int       `json:"matched"`
	Capped           bool      `json:"capped"`
	Cap              int       `json:"cap"`
	OpTotalMs        float64   `json:"opTotalMs"`
	OutsideSpans     int       `json:"outsideSpans"`
	IncompleteTraces int       `json:"incompleteTraces"`
	TruncatedTraces  int       `json:"truncatedTraces"`
	NodesTruncated   bool      `json:"nodesTruncated"`
	Nodes            []OpNode  `json:"nodes"` // pre-order; never null
}

// opRoot is a candidate operation root.
type opRoot struct {
	traceID, spanID, parent string
	time                    time.Time
	seq                     int
}

// later reports whether a starts after b (Seq breaks a tie).
func (a opRoot) later(b opRoot) bool {
	if !a.time.Equal(b.time) {
		return a.time.After(b.time)
	}
	return a.seq > b.seq
}

// merged accumulates one OpNode.
type merged struct {
	parent    int // -1 for the root
	name      string
	kids      []int
	durs      []float64
	selfMs    float64
	startSum  float64
	errors    int
	types     map[string]int
	services  map[string]bool
	traces    int
	lastTrace int // index of the last trace that counted in traces
	serial    int
	serialMs  float64
	slow      Slowest
	slowTrace int
	slowSeq   int
}

// Aggregate merges the traces whose operation root (a span named o.Name whose
// parent is absent or not in recs) matches o. One root per trace, the
// earliest; the most recent o.Cap traces. It reports false when no root
// matches.
func Aggregate(recs []record.Record, o AggOptions) (Operation, bool) {
	limit := o.Cap
	if limit <= 0 {
		limit = AggCap
	}
	anyService := o.Service == "" || o.Service == "*"

	// Pass 1: every span ID, and the candidate roots.
	ids := map[string]bool{}
	var cands []opRoot
	for i := range recs {
		r := &recs[i]
		if r.Source != record.SourceOtel {
			continue
		}
		tid, sid := record.TraceRef(r.Fields)
		if sid == "" {
			continue
		}
		ids[sid] = true
		if tid == "" {
			continue
		}
		if name, _ := r.Fields["name"].(string); name != o.Name {
			continue
		}
		if !anyService && service(r) != o.Service {
			continue
		}
		if (!o.From.IsZero() && r.Time.Before(o.From)) || (!o.To.IsZero() && r.Time.After(o.To)) {
			continue
		}
		p, _ := r.Fields["parent_span_id"].(string)
		cands = append(cands, opRoot{tid, sid, record.NormalizeSpanID(p), r.Time, r.Seq})
	}
	first := map[string]opRoot{}
	for _, c := range cands {
		if c.parent != "" && c.parent != c.spanID && ids[c.parent] {
			continue // a nested call
		}
		if f, ok := first[c.traceID]; !ok || f.later(c) {
			first[c.traceID] = c
		}
	}
	if len(first) == 0 {
		return Operation{}, false
	}
	roots := make([]opRoot, 0, len(first))
	for _, c := range first {
		roots = append(roots, c)
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].later(roots[j]) })
	out := Operation{Name: o.Name, Service: o.Service, From: o.From, To: o.To, Matched: len(roots), Cap: limit, Nodes: []OpNode{}}
	if len(roots) > limit {
		roots, out.Capped = roots[:limit], true
	}

	// Pass 2: the span records of the kept traces.
	slot := make(map[string]int, len(roots))
	for i, c := range roots {
		slot[c.traceID] = i
	}
	groups := make([][]record.Record, len(roots))
	for i := range recs {
		r := &recs[i]
		if r.Source != record.SourceOtel {
			continue
		}
		tid, _ := record.TraceRef(r.Fields)
		if k, ok := slot[tid]; ok {
			groups[k] = append(groups[k], *r)
		}
	}

	// Merge each trace's operation subtree, most recent trace first.
	var ms []*merged
	index := map[string]int{} // parent index + "\x00" + name -> merged index
	for ti, c := range roots {
		tr, ok := Build(groups[ti], c.traceID, Limit)
		groups[ti] = nil
		if !ok {
			continue
		}
		if tr.Truncated {
			out.TruncatedTraces++
		}
		ri := -1
		spans := 0
		for i := range tr.Nodes {
			if tr.Nodes[i].Kind == KindSpan {
				spans++
				if tr.Nodes[i].ID == c.spanID {
					ri = i
				}
			}
		}
		if ri < 0 {
			continue // the root fell past Build's limit
		}
		serial := map[string]bool{}
		for _, run := range tr.SerialRuns {
			for _, id := range run.Spans {
				serial[id] = true
			}
		}
		root := tr.Nodes[ri]
		out.Traces++
		out.OpTotalMs += root.DurationMs
		at := map[string]int{} // node ID -> merged index; -1 when dropped
		inside := 0
		for i := ri; i < len(tr.Nodes); i++ {
			n := &tr.Nodes[i]
			if i > ri && n.Depth <= root.Depth {
				break
			}
			if n.Kind != KindSpan {
				continue
			}
			inside++
			p := -1
			if i > ri {
				var ok bool
				if p, ok = at[n.Parent]; !ok || p < 0 {
					at[n.ID] = -1
					continue
				}
			}
			key := strconv.Itoa(p) + "\x00" + n.Name
			m, ok := index[key]
			if !ok {
				if len(ms) >= Limit {
					out.NodesTruncated = true
					at[n.ID] = -1
					continue
				}
				m = len(ms)
				index[key] = m
				ms = append(ms, &merged{parent: p, name: n.Name, types: map[string]int{}, services: map[string]bool{}, lastTrace: -1})
				if p >= 0 {
					ms[p].kids = append(ms[p].kids, m)
				}
			}
			at[n.ID] = m
			g := ms[m]
			if g.lastTrace != ti {
				g.lastTrace = ti
				g.traces++
			}
			g.durs = append(g.durs, n.DurationMs)
			g.selfMs += n.SelfMs
			g.startSum += n.StartMs - root.StartMs
			if n.Error {
				g.errors++
			}
			g.types[n.Type]++
			if n.Service != "" {
				g.services[n.Service] = true
			}
			if serial[n.ID] {
				g.serial++
				g.serialMs += n.DurationMs
			}
			if len(g.durs) == 1 || n.DurationMs > g.slow.DurationMs ||
				(n.DurationMs == g.slow.DurationMs && (ti < g.slowTrace || ti == g.slowTrace && n.Seq < g.slowSeq)) {
				g.slow = Slowest{TraceID: c.traceID, SpanID: n.ID, DurationMs: n.DurationMs}
				g.slowTrace, g.slowSeq = ti, n.Seq
			}
		}
		if outside := spans - inside; outside > 0 {
			out.OutsideSpans += outside
			out.IncompleteTraces++
		}
	}
	if len(ms) == 0 {
		return Operation{}, false
	}

	// Emit in pre-order: children by mean start offset, then name.
	pct := func(v float64) float64 {
		if out.OpTotalMs <= 0 {
			return 0
		}
		return finite(v / out.OpTotalMs * 100)
	}
	var emit func(m int, parent string, depth int)
	emit = func(m int, parent string, depth int) {
		g := ms[m]
		sort.SliceStable(g.kids, func(i, j int) bool {
			a, b := ms[g.kids[i]], ms[g.kids[j]]
			sa, sb := a.startSum/float64(len(a.durs)), b.startSum/float64(len(b.durs))
			if sa != sb {
				return sa < sb
			}
			return a.name < b.name
		})
		calls := len(g.durs)
		sort.Float64s(g.durs)
		total := 0.0
		for _, d := range g.durs {
			total += d
		}
		n := OpNode{
			ID:             "n" + strconv.Itoa(len(out.Nodes)),
			Parent:         parent,
			Depth:          depth,
			Children:       len(g.kids),
			Name:           g.name,
			Type:           mostCommon(g.types),
			Services:       sortedKeys(g.services),
			Traces:         g.traces,
			Calls:          calls,
			CallsPerTrace:  finite(float64(calls) / float64(g.traces)),
			P50Ms:          finite(nearestRank(g.durs, 50)),
			P95Ms:          finite(nearestRank(g.durs, 95)),
			TotalMs:        finite(total),
			SelfMs:         finite(g.selfMs),
			TotalPct:       pct(total),
			SelfPct:        pct(g.selfMs),
			Errors:         g.errors,
			SerialCalls:    g.serial,
			SerialMs:       finite(g.serialMs),
			SerialPerTrace: finite(float64(g.serial) / float64(g.traces)),
			SerialPct:      pct(g.serialMs),
			Slowest:        g.slow,
		}
		n.Slowest.DurationMs = finite(n.Slowest.DurationMs)
		out.Nodes = append(out.Nodes, n)
		id := n.ID
		for _, k := range g.kids {
			emit(k, id, depth+1)
		}
	}
	emit(0, "", 0)
	out.OpTotalMs = finite(out.OpTotalMs)
	return out, true
}

// nearestRank is the p-th percentile (0–100) of sorted: sorted[ceil(p/100·n)−1].
func nearestRank(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	i := int(math.Ceil(p/100*float64(n))) - 1
	return sorted[min(max(i, 0), n-1)]
}

// mostCommon is the key with the highest count; ties go to the
// alphabetically first.
func mostCommon(c map[string]int) string {
	best, bestN := "", 0
	for k, n := range c {
		if n > bestN || (n == bestN && k < best) {
			best, bestN = k, n
		}
	}
	return best
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
