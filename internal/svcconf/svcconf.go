// Package svcconf locates and writes the per-service config directories:
// <base>/services/<escaped service key>/.
package svcconf

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Base returns the loghorn config base: xdg/loghorn when xdg is set, else
// home/.config/loghorn. It never walks the tree and never creates anything.
func Base(xdg, home string) (string, error) {
	if xdg != "" {
		return filepath.Join(xdg, "loghorn"), nil
	}
	if home != "" {
		return filepath.Join(home, ".config", "loghorn"), nil
	}
	return "", errors.New("cannot locate the config directory: neither XDG_CONFIG_HOME nor the home directory is set")
}

// BaseFromEnv is Base for the current environment.
func BaseFromEnv() (string, error) {
	home, _ := os.UserHomeDir()
	return Base(os.Getenv("XDG_CONFIG_HOME"), home)
}

const hexDigits = "0123456789ABCDEF"

// EscapeKey turns a service key into a single safe path element. The empty
// key is "default". Bytes [A-Za-z0-9._-] are kept, except a leading '.', which
// is escaped so the element can never be "." or ".." nor a hidden directory;
// every other byte becomes %XX.
func EscapeKey(key string) string {
	if key == "" {
		return "default"
	}
	var b strings.Builder
	for i := 0; i < len(key); i++ {
		c := key[i]
		keep := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '_' || c == '-' || (c == '.' && i > 0)
		if keep {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&15])
	}
	return b.String()
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// UnescapeKey reverses EscapeKey. "default" maps to "default" (the empty key
// is not recoverable, by design).
func UnescapeKey(s string) (string, error) {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			out = append(out, s[i])
			continue
		}
		if i+2 >= len(s) {
			return "", fmt.Errorf("bad escape in %q", s)
		}
		hi, ok1 := unhex(s[i+1])
		lo, ok2 := unhex(s[i+2])
		if !ok1 || !ok2 {
			return "", fmt.Errorf("bad escape in %q", s)
		}
		out = append(out, hi<<4|lo)
		i += 2
	}
	return string(out), nil
}

// Dir is the config directory of a service under base.
func Dir(base, key string) string {
	return filepath.Join(base, "services", EscapeKey(key))
}

// WriteAtomic writes path via a temp file in the same directory and a rename,
// creating the directory first.
func WriteAtomic(path string, encode func(io.Writer) error) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".write-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := encode(tmp); err != nil {
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
	return os.Rename(tmp.Name(), path)
}
