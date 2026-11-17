package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func pickKeys(p picker, keys ...string) picker {
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "up":
			msg = tea.KeyMsg{Type: tea.KeyUp}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		m, _ := p.Update(msg)
		p = m.(picker)
	}
	return p
}

func TestPickerMovesClampsAndChooses(t *testing.T) {
	p := newPicker("jobs", []string{"a", "b", "c"})
	p = pickKeys(p, "up", "j", "down", "down", "down")
	if p.cursor != 2 {
		t.Fatalf("cursor = %d, want clamped to 2", p.cursor)
	}
	p = pickKeys(p, "k", "enter")
	if !p.chosen || p.cursor != 1 {
		t.Fatalf("chosen=%v cursor=%d, want b chosen", p.chosen, p.cursor)
	}
	if p.View() != "" {
		t.Fatalf("a finished picker should erase itself, got %q", p.View())
	}
}

func TestPickerCancel(t *testing.T) {
	for _, k := range []string{"q", "esc"} {
		if p := pickKeys(newPicker("jobs", []string{"a"}), k); p.chosen || !p.finished {
			t.Fatalf("%s: chosen=%v finished=%v, want cancelled", k, p.chosen, p.finished)
		}
	}
}

// A list longer than the terminal scrolls to keep the cursor in view.
func TestPickerScrollsToTheCursor(t *testing.T) {
	items := []string{"i0", "i1", "i2", "i3", "i4", "i5", "i6", "i7"}
	m, _ := newPicker("jobs", items).Update(tea.WindowSizeMsg{Width: 40, Height: 6}) // 3 rows
	p := pickKeys(m.(picker), "G")
	v := p.View()
	if !strings.Contains(v, "i7") || strings.Contains(v, "i4") || !strings.Contains(v, "8 of 8") {
		t.Fatalf("view at the end should show i5–i7 and the position:\n%s", v)
	}
	p = pickKeys(p, "g")
	if v := p.View(); !strings.Contains(v, "i0") || strings.Contains(v, "i3") {
		t.Fatalf("view at the top should show i0–i2:\n%s", v)
	}
}

// A burst of keys arrives as one message with several runes; each counts.
func TestPickerTakesABurstOfKeysOneByOne(t *testing.T) {
	p := pickKeys(newPicker("jobs", []string{"a", "b", "c", "d"}), "jjj")
	if p.cursor != 3 {
		t.Fatalf("cursor = %d after \"jjj\", want 3", p.cursor)
	}
	if p = pickKeys(newPicker("jobs", []string{"a", "b"}), "jqj"); !p.finished || p.chosen || p.cursor != 1 {
		t.Fatalf("q mid-burst should cancel and stop there: %+v", p)
	}
}

// A cursor that starts past the first screenful is scrolled into view once
// the terminal's size is known.
func TestPickerStartingCursorIsScrolledIntoView(t *testing.T) {
	items := []string{"i0", "i1", "i2", "i3", "i4", "i5", "i6", "i7"}
	p := newPicker("jobs", items)
	p.cursor = 6
	m, _ := p.Update(tea.WindowSizeMsg{Width: 40, Height: 6}) // 3 rows
	if v := m.(picker).View(); !strings.Contains(v, "> i6") || strings.Contains(v, "i3") {
		t.Fatalf("view should show the starting cursor on i6:\n%s", v)
	}
}
