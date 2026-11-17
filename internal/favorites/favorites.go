// Package favorites keeps named queries in the repo's .config/loghorn/queries.toml.
package favorites

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	"github.com/jimbeveridge/loghorn/internal/query"
)

// MaxNameLen is the longest favorite name accepted, in characters.
const MaxNameLen = 100

var (
	// ErrExists is returned by Put when the name is taken and overwrite is false.
	ErrExists = errors.New("favorite already exists")
	// ErrNotFound is returned by Delete when no favorite has the name.
	ErrNotFound = errors.New("favorite not found")
	// ErrBadName is returned by Put for an empty or over-long name.
	ErrBadName = fmt.Errorf("favorite name must be 1 to %d characters", MaxNameLen)
)

// Favorite is a named query.
type Favorite struct {
	Name  string      `toml:"name" json:"name"`
	Query query.Query `toml:"query" json:"query"`
}

type file struct {
	Favorites []Favorite `toml:"favorite"`
}

func path(dir string) string { return filepath.Join(dir, "queries.toml") }

// Load reads dir/queries.toml in file order. An absent file is an empty list.
func Load(dir string) ([]Favorite, error) {
	data, err := os.ReadFile(path(dir))
	if errors.Is(err, os.ErrNotExist) {
		return []Favorite{}, nil
	}
	if err != nil {
		return nil, err
	}
	var f file
	if err := toml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path(dir), err)
	}
	if f.Favorites == nil {
		f.Favorites = []Favorite{}
	}
	return f.Favorites, nil
}

// Put adds f, or replaces the favorite of the same name when overwrite is
// true (keeping its position). Callers serialise concurrent writes.
func Put(dir string, f Favorite, overwrite bool) error {
	if f.Name == "" || utf8.RuneCountInString(f.Name) > MaxNameLen {
		return ErrBadName
	}
	favs, err := Load(dir)
	if err != nil {
		return err
	}
	for i := range favs {
		if favs[i].Name == f.Name {
			if !overwrite {
				return ErrExists
			}
			favs[i] = f
			return write(dir, favs)
		}
	}
	return write(dir, append(favs, f))
}

// Delete removes the favorite with the given name.
func Delete(dir, name string) error {
	favs, err := Load(dir)
	if err != nil {
		return err
	}
	for i := range favs {
		if favs[i].Name == name {
			return write(dir, append(favs[:i:i], favs[i+1:]...))
		}
	}
	return ErrNotFound
}

// write replaces dir/queries.toml via a temp file and rename in dir.
func write(dir string, favs []Favorite) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".queries-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := toml.NewEncoder(tmp).Encode(file{Favorites: favs}); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path(dir))
}
