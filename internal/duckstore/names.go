package duckstore

import (
	"strconv"
	"strings"
)

// namer gives each real name a DuckDB identifier. DuckDB compares
// identifiers without regard to case, so a name equal to a taken one except
// for case gets a "~2", "~3"… suffix; names are never merged.
type namer struct {
	byName map[string]string
	taken  map[string]bool // lower-cased identifiers in use
}

func newNamer(reserved ...string) *namer {
	n := &namer{byName: map[string]string{}, taken: map[string]bool{}}
	for _, r := range reserved {
		n.taken[strings.ToLower(r)] = true
	}
	return n
}

// name returns real's identifier, allocating one on first use.
func (n *namer) name(real string) string {
	if p, ok := n.byName[real]; ok {
		return p
	}
	base := real
	if base == "" {
		base = "~empty"
	}
	p := base
	for i := 2; n.taken[strings.ToLower(p)]; i++ {
		p = base + "~" + strconv.Itoa(i)
	}
	n.set(real, p)
	return p
}

// set records an identifier already allocated (loaded from metadata).
func (n *namer) set(real, ident string) {
	n.byName[real] = ident
	n.taken[strings.ToLower(ident)] = true
}

func (n *namer) lookup(real string) (string, bool) {
	p, ok := n.byName[real]
	return p, ok
}

func itoa(i int) string { return strconv.Itoa(i) }
