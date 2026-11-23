package datadir

import (
	"path/filepath"
	"testing"
)

func TestForOS(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	for _, c := range []struct {
		name, goos string
		env        map[string]string
		home, want string
	}{
		{"darwin", "darwin", nil, "/Users/u", "/Users/u/Library/Application Support/loghorn"},
		{"linux xdg", "linux", map[string]string{"XDG_STATE_HOME": "/s"}, "/home/u", "/s/loghorn"},
		{"linux default", "linux", nil, "/home/u", "/home/u/.local/state/loghorn"},
		{"windows", "windows", map[string]string{"LOCALAPPDATA": `C:\L`}, `C:\Users\u`, filepath.Join(`C:\L`, "loghorn")},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := forOS(c.goos, env(c.env), c.home); got != c.want {
				t.Fatalf("forOS = %q; want %q", got, c.want)
			}
		})
	}
}

func TestDirEnvOverride(t *testing.T) {
	t.Setenv("LOGHORN_DATA_DIR", "/x")
	got, err := Dir()
	if err != nil || got != "/x" {
		t.Fatalf("Dir = %q, %v; want /x", got, err)
	}
}
