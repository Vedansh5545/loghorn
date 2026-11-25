package browser

import (
	"slices"
	"testing"
)

func TestCommand(t *testing.T) {
	const u = "http://127.0.0.1:4380/"
	for goos, want := range map[string][]string{
		"darwin":  {"open", u},
		"linux":   {"xdg-open", u},
		"freebsd": {"xdg-open", u},
		"windows": {"rundll32", "url.dll,FileProtocolHandler", u},
	} {
		if got := command(goos, u).Args; !slices.Equal(got, want) {
			t.Errorf("%s: %q; want %q", goos, got, want)
		}
	}
}
