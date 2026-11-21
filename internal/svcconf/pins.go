package svcconf

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"github.com/jimbeveridge/loghorn/internal/record"
)

// ErrBadPin is returned for a pin with an unknown source or an empty kind.
var ErrBadPin = errors.New("bad pin: unknown source or empty kind")

// Pin is a kind pinned to the top of a service's list.
type Pin struct {
	Source string `toml:"source" json:"source"`
	Kind   string `toml:"kind" json:"kind"`
}

type pinsFile struct {
	Pins []Pin `toml:"pin"`
}

// LoadPins reads dir/pins.toml; an absent file is an empty, non-nil list.
func LoadPins(dir string) ([]Pin, error) {
	path := filepath.Join(dir, "pins.toml")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []Pin{}, nil
	}
	if err != nil {
		return nil, err
	}
	var f pinsFile
	if err := toml.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	if f.Pins == nil {
		f.Pins = []Pin{}
	}
	return f.Pins, nil
}

// SavePins validates pins, drops duplicates (first wins) and writes
// dir/pins.toml atomically.
func SavePins(dir string, pins []Pin) error {
	seen := map[Pin]bool{}
	out := []Pin{}
	for _, p := range pins {
		src, ok := record.ParseSource(p.Source)
		if !ok || p.Kind == "" {
			return ErrBadPin
		}
		p.Source = src
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return WriteAtomic(filepath.Join(dir, "pins.toml"), func(w io.Writer) error {
		return toml.NewEncoder(w).Encode(pinsFile{Pins: out})
	})
}
