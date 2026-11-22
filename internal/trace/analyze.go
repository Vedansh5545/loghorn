package trace

import (
	"sort"
)

const eps = 1e-9

// analyze fills SelfMs, Critical, SelfTime and SerialRuns of t. Nodes are in
// pre-order, so every node's children follow it.
func analyze(t *Trace) {
	kids := map[string][]int{}
	var tops []int
	for i := range t.Nodes {
		if p := t.Nodes[i].Parent; p != "" {
			kids[p] = append(kids[p], i)
		} else {
			tops = append(tops, i)
		}
	}
	end := func(i int) float64 { return t.Nodes[i].StartMs + t.Nodes[i].DurationMs }

	// Self time: duration minus the union of the child spans' intervals,
	// clipped to the span.
	for i := range t.Nodes {
		n := &t.Nodes[i]
		if n.Kind != KindSpan {
			continue
		}
		type iv struct{ s, e float64 }
		var ivs []iv
		for _, k := range kids[n.ID] {
			if t.Nodes[k].Kind != KindSpan {
				continue
			}
			s, e := max(t.Nodes[k].StartMs, n.StartMs), min(end(k), end(i))
			if e > s {
				ivs = append(ivs, iv{s, e})
			}
		}
		sort.Slice(ivs, func(a, b int) bool { return ivs[a].s < ivs[b].s })
		covered, curS, curE := 0.0, 0.0, 0.0
		open := false
		for _, v := range ivs {
			if open && v.s <= curE {
				curE = max(curE, v.e)
				continue
			}
			if open {
				covered += curE - curS
			}
			curS, curE, open = v.s, v.e, true
		}
		if open {
			covered += curE - curS
		}
		n.SelfMs = finite(max(n.DurationMs-covered, 0))
	}

	// Summary by span name.
	byName := map[string]*NameTime{}
	for _, n := range t.Nodes {
		if n.Kind != KindSpan {
			continue
		}
		nt := byName[n.Name]
		if nt == nil {
			nt = &NameTime{Name: n.Name}
			byName[n.Name] = nt
		}
		nt.Count++
		nt.TotalMs += n.DurationMs
		nt.SelfMs += n.SelfMs
	}
	t.SelfTime = []NameTime{}
	for _, nt := range byName {
		t.SelfTime = append(t.SelfTime, *nt)
	}
	sort.Slice(t.SelfTime, func(a, b int) bool {
		x, y := t.SelfTime[a], t.SelfTime[b]
		if x.SelfMs != y.SelfMs {
			return x.SelfMs > y.SelfMs
		}
		return x.Name < y.Name
	})

	// Critical path.
	start := -1
	for _, i := range tops {
		if isLeaf(t.Nodes[i].Kind) {
			continue
		}
		if start < 0 || end(i) > end(start) {
			start = i
		}
	}
	var walk func(i int)
	walk = func(i int) {
		if t.Nodes[i].Kind == KindSpan {
			t.Nodes[i].Critical = true
		}
		var cs []int
		for _, k := range kids[t.Nodes[i].ID] {
			if !isLeaf(t.Nodes[k].Kind) {
				cs = append(cs, k)
			}
		}
		sort.SliceStable(cs, func(a, b int) bool {
			x, y := cs[a], cs[b]
			if end(x) != end(y) {
				return end(x) > end(y)
			}
			if t.Nodes[x].StartMs != t.Nodes[y].StartMs {
				return t.Nodes[x].StartMs > t.Nodes[y].StartMs
			}
			return t.Nodes[x].Seq < t.Nodes[y].Seq
		})
		cursor := end(i)
		for _, k := range cs {
			if end(k) <= cursor+eps {
				walk(k)
				cursor = t.Nodes[k].StartMs
			}
		}
	}
	if start >= 0 {
		walk(start)
	}

	// Serial runs of db spans, per parent, in pre-order of the first span.
	t.SerialRuns = []SerialRun{}
	var runs []SerialRun
	flush := func(parent string, run []int) {
		if len(run) < 3 {
			return
		}
		r := SerialRun{Parent: parent, First: t.Nodes[run[0]].ID, Count: len(run),
			WallMs: finite(end(run[len(run)-1]) - t.Nodes[run[0]].StartMs)}
		for _, i := range run {
			r.Spans = append(r.Spans, t.Nodes[i].ID)
		}
		runs = append(runs, r)
	}
	scan := func(parent string, idx []int) {
		var sp []int
		for _, i := range idx {
			if t.Nodes[i].Kind == KindSpan {
				sp = append(sp, i)
			}
		}
		sort.SliceStable(sp, func(a, b int) bool { return t.Nodes[sp[a]].StartMs < t.Nodes[sp[b]].StartMs })
		var run []int
		for _, i := range sp {
			if t.Nodes[i].Type != "db" {
				flush(parent, run)
				run = nil
				continue
			}
			if len(run) > 0 && t.Nodes[i].StartMs < end(run[len(run)-1])-eps {
				flush(parent, run)
				run = nil
			}
			run = append(run, i)
		}
		flush(parent, run)
	}
	scan("", tops)
	for i := range t.Nodes {
		if ks := kids[t.Nodes[i].ID]; len(ks) > 0 {
			scan(t.Nodes[i].ID, ks)
		}
	}
	index := map[string]int{}
	for i := range t.Nodes {
		index[t.Nodes[i].ID] = i
	}
	sort.SliceStable(runs, func(a, b int) bool { return index[runs[a].First] < index[runs[b].First] })
	t.SerialRuns = append(t.SerialRuns, runs...)
}
