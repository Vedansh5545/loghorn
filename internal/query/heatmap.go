package query

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/schema"
)

// Heatmap is span count by time bucket and log-duration band.
type Heatmap struct {
	Bucket        string      `json:"bucket"` // as Graph.Bucket
	BucketSeconds float64     `json:"bucketSeconds"`
	Starts        []time.Time `json:"starts"` // as Graph.Starts
	Bands         []Band      `json:"bands"`  // ascending
	Counts        [][]int     `json:"counts"` // [timeIndex][bandIndex]
	Max           int         `json:"max"`    // largest cell count
	Total         int         `json:"total"`  // spans in the grid
	Skipped       int         `json:"skipped"`
}

// Band is one duration band in ms: Lo inclusive, Hi exclusive. A band's Lo is
// the previous band's Hi, bit for bit.
type Band struct {
	Lo float64 `json:"lo"`
	Hi float64 `json:"hi"`
}

// HeatCell narrows a query to one heatmap cell: From <= time < To and
// Lo <= duration_ms < Hi on the raw number. It is not saved with a favorite.
type HeatCell struct {
	From time.Time `json:"from"`         // inclusive
	To   time.Time `json:"to"`           // exclusive
	Lo   *float64  `json:"lo,omitempty"` // ms, inclusive; nil = no lower bound
	Hi   float64   `json:"hi"`           // ms, exclusive
}

func (c *HeatCell) validate() error {
	if !c.To.After(c.From) {
		return fmt.Errorf("cell: to must be after from")
	}
	if c.Lo != nil && !(*c.Lo < c.Hi) {
		return fmt.Errorf("cell: lo must be below hi")
	}
	return nil
}

// contains reports whether r falls in the cell.
func (c *HeatCell) contains(r *record.Record) bool {
	if r.Time.IsZero() || r.Time.Before(c.From) || !r.Time.Before(c.To) {
		return false
	}
	d, ok := durationMs(r)
	return ok && (c.Lo == nil || d >= *c.Lo) && d < c.Hi
}

const (
	bandsPerDecade = 5
	// minBandMs and maxBandMs bound the grid at 1ns and about 30 years, so
	// one wild outlier cannot add hundreds of bands (or an infinite edge).
	// Shorter durations land in the lowest band; longer ones are skipped.
	minBandMs = 1e-6
	maxBandMs = 1e12
)

var durationField = schema.Field{Path: "duration_ms", Type: schema.TFloat}

// durationMs is the raw duration_ms as a number, whatever the schema says, so
// an int-inferred or retyped field still bins.
func durationMs(r *record.Record) (float64, bool) {
	v := schema.Convert(r.Fields[durationField.Path], durationField)
	if v.Bad || v.Empty {
		return 0, false
	}
	return v.Num, true
}

// decadeAtOrBelow is the largest d with 10^d <= x, for x > 0. It checks
// against math.Pow, the function the band edges use, so float error in
// Log10 never puts an exact decade on the wrong side.
func decadeAtOrBelow(x float64) int {
	d := int(math.Floor(math.Log10(x)))
	for math.Pow(10, float64(d)) > x {
		d--
	}
	for math.Pow(10, float64(d+1)) <= x {
		d++
	}
	return d
}

// bands spans the decade at or below minMs to the first decade above maxMs
// (at least one decade), five bands per decade with edges 10^(k/5) ms. A
// minMs <= 0 means 1ms.
func bands(minMs, maxMs float64) []Band {
	if !(minMs > 0) || math.IsInf(minMs, 0) {
		minMs = 1
	}
	minMs = min(max(minMs, minBandMs), maxBandMs)
	maxMs = min(maxMs, maxBandMs)
	if !(maxMs >= minMs) {
		maxMs = minMs
	}
	lo := decadeAtOrBelow(minMs)
	hi := max(decadeAtOrBelow(maxMs)+1, lo+1)
	edges := make([]float64, 0, (hi-lo)*bandsPerDecade+1)
	for k := lo * bandsPerDecade; k <= hi*bandsPerDecade; k++ {
		edges = append(edges, math.Pow(10, float64(k)/bandsPerDecade))
	}
	out := make([]Band, len(edges)-1)
	for i := range out {
		out[i] = Band{Lo: edges[i], Hi: edges[i+1]}
	}
	return out
}

// runHeatmap counts the matched spans by time bucket (the graph's x-axis)
// and duration band. Spans with no time, no duration_ms or a duration past
// maxBandMs are skipped; durations below the lowest band, zero and negative
// ones included, count in the lowest band.
func runHeatmap(matched []*record.Record, q Query, from, to time.Time, loc *time.Location) (*Heatmap, error) {
	var timed []*record.Record
	var durs []float64
	h := &Heatmap{}
	minPos, maxD := math.Inf(1), math.Inf(-1)
	for _, r := range matched {
		d, ok := durationMs(r)
		if !ok || r.Time.IsZero() {
			h.Skipped++
			continue
		}
		timed = append(timed, r)
		durs = append(durs, d)
		if d > 0 {
			minPos = min(minPos, d)
		}
		maxD = max(maxD, d)
	}
	if math.IsInf(minPos, 1) {
		minPos = 1
	}
	starts, size, name, _, err := xAxis(timed, q.Bucket, from, to, loc)
	if err != nil {
		return nil, err
	}
	h.Bucket, h.BucketSeconds, h.Starts = name, size.Seconds(), starts
	h.Bands = bands(minPos, maxD)
	h.Counts = make([][]int, len(starts))
	for i := range h.Counts {
		h.Counts[i] = make([]int, len(h.Bands))
	}
	for i, r := range timed {
		ti := bucketIndex(starts, r.Time)
		bi := sort.Search(len(h.Bands), func(j int) bool { return h.Bands[j].Hi > durs[i] })
		if bi == len(h.Bands) { // past maxBandMs
			h.Skipped++
			continue
		}
		h.Counts[ti][bi]++
		h.Total++
		h.Max = max(h.Max, h.Counts[ti][bi])
	}
	return h, nil
}
