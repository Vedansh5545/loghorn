package tui

import (
	"fmt"
	"io"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// The picker runs before the terminal's background is known (SetBackground),
// so it styles with reverse video and faint text, which suit any theme.
var (
	pickSel  = lipgloss.NewStyle().Reverse(true)
	pickHint = lipgloss.NewStyle().Faint(true)
)

// Pick asks the user to choose one of items before the main TUI starts, as
// -gh-run does to pick a job from a run. It draws inline on out (not the alt
// screen), reads keys from in, and erases itself when done. ok is false if
// the user cancelled with q, esc or ctrl+c. The cursor starts on initial.
func Pick(title string, items []string, initial int, in io.Reader, out io.Writer) (index int, ok bool, err error) {
	p := newPicker(title, items)
	p.cursor = min(max(initial, 0), len(items)-1)
	final, err := tea.NewProgram(p, tea.WithInput(in), tea.WithOutput(out)).Run()
	if err != nil {
		return 0, false, err
	}
	p = final.(picker)
	return p.cursor, p.chosen, nil
}

// picker is Pick's model: a cursor over items, scrolled to fit the terminal.
type picker struct {
	title    string
	items    []string
	cursor   int
	offset   int // first item shown
	height   int // terminal rows; 0 until the first WindowSizeMsg
	chosen   bool
	finished bool
}

func newPicker(title string, items []string) picker {
	return picker{title: title, items: items}
}

func (p picker) Init() tea.Cmd { return nil }

// rows is how many items fit: the terminal less the title and footer lines.
func (p picker) rows() int {
	if p.height == 0 {
		return len(p.items)
	}
	return max(p.height-3, 1)
}

func (p picker) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		p.height = msg.Height
	case tea.KeyMsg:
		// Keys typed in a burst (fast typing, key repeat) can arrive as one
		// message carrying several runes; take them one at a time.
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 {
			var m tea.Model = p
			var cmd tea.Cmd
			for _, r := range msg.Runes {
				m, cmd = m.(picker).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
				if m.(picker).finished {
					break
				}
			}
			return m, cmd
		}
		switch msg.String() {
		case "up", "k":
			p.cursor--
		case "down", "j":
			p.cursor++
		case "pgup", "ctrl+u":
			p.cursor -= p.rows()
		case "pgdown", "ctrl+d":
			p.cursor += p.rows()
		case "home", "g":
			p.cursor = 0
		case "end", "G":
			p.cursor = len(p.items) - 1
		case "enter":
			p.chosen, p.finished = true, true
			return p, tea.Quit
		case "q", "esc", "ctrl+c":
			p.finished = true
			return p, tea.Quit
		}
	}
	p.cursor = min(max(p.cursor, 0), len(p.items)-1)
	if p.cursor < p.offset {
		p.offset = p.cursor
	}
	if n := p.rows(); p.cursor >= p.offset+n {
		p.offset = p.cursor - n + 1
	}
	return p, nil
}

func (p picker) View() string {
	if p.finished {
		return "" // leave the terminal as it was found
	}
	var b strings.Builder
	b.WriteString(p.title + "\n")
	end := min(p.offset+p.rows(), len(p.items))
	for i := p.offset; i < end; i++ {
		if i == p.cursor {
			b.WriteString(pickSel.Render("> "+p.items[i]) + "\n")
		} else {
			b.WriteString("  " + p.items[i] + "\n")
		}
	}
	more := ""
	if end-p.offset < len(p.items) {
		more = fmt.Sprintf(" · %d of %d", p.cursor+1, len(p.items))
	}
	b.WriteString(pickHint.Render("↑/↓ or j/k move · enter choose · q cancel" + more))
	return b.String()
}
