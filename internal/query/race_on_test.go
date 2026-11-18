//go:build race

package query

// raceEnabled reports whether the race detector is on. It slows the query
// several-fold, so wall-clock budgets are not checked under it.
const raceEnabled = true
