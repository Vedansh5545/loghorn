package tui

import (
	"strings"
	"testing"
)

func TestStatusBarShowsWebURL(t *testing.T) {
	m := NewModel(nil, 100)
	m.width, m.height = 120, 12
	m.SetWebURL("http://127.0.0.1:1234/")
	m = imps(m, 2)
	if got := m.statusBar(); !strings.Contains(got, "web http://127.0.0.1:1234/") {
		t.Fatalf("bar should show the web URL:\n%s", got)
	}
}
