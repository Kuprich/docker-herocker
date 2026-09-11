package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/kuri4/dockerherocker/config"
)

var t = config.DefaultTheme()

// selTextStyle is the app-owned text highlight (reжим A mouse selection),
// matching the container-table selected-row background so it reads as a
// selection instead of a paint artifact.
var selTextStyle = lipgloss.NewStyle().
	Background(lipgloss.Color("#2d4a2e")).
	Foreground(t.Foreground)

// CopyToastStyle is the transient "Copied to clipboard" pill drawn at the
// right edge of the tab bar after an OSC 52 copy. It mirrors the active-tab
// palette so it reads as a positive status rather than chrome.
var CopyToastStyle = lipgloss.NewStyle().
	Background(lipgloss.Color("#2ea043")).
	Foreground(t.Background).
	Bold(true).
	Padding(0, 1)

// MenuBoxStyle is the frame of the container right-click popup: rows and
// border share the surface background so the popup blocks whatever the app
// drew underneath it (no default-terminal cells can leak through).
var MenuBoxStyle = lipgloss.NewStyle().
	Background(t.Surface).
	Foreground(t.Muted)

// MenuItemStyle paints a regular (unselected) popup row.
var MenuItemStyle = lipgloss.NewStyle().
	Background(t.Surface).
	Foreground(t.Foreground)

// MenuActiveItemStyle is the highlighted popup row, mirroring the active-tab
// palette (inverted green) so the cursor position reads instantly.
var MenuActiveItemStyle = lipgloss.NewStyle().
	Background(lipgloss.Color("#2ea043")).
	Foreground(t.Background).
	Bold(true)

// MenuKeyStyle paints the single-letter hotkey inside a menu row, using the
// accent green on the surface so it pops against the row text.
var MenuKeyStyle = lipgloss.NewStyle().
	Background(t.Surface).
	Foreground(t.Accent)

// MenuActiveKeyStyle is the hotkey on the selected row. The selected row uses
// an inverted-green background, so a green or amber letter would vanish (green
// on green). A bright white letter keeps the key readable and distinct from
// the dark label text on that row.
var MenuActiveKeyStyle = lipgloss.NewStyle().
	Background(lipgloss.Color("#2ea043")).
	Foreground(t.Foreground).
	Bold(true)

// MenuCliStyle paints the (right-aligned) docker CLI hint beside an action:
// muted so it stays secondary next to the row label, on the row background.
var MenuCliStyle = lipgloss.NewStyle().
	Background(t.Surface).
	Foreground(t.Muted)

// MenuActiveCliStyle is the docker CLI hint on the selected (inverted-green)
// row. The muted gray used on surface rows is too dim against the green, so
// the hint switches to the bright foreground (still non-bold, so it does not
// compete with the bold label).
var MenuActiveCliStyle = lipgloss.NewStyle().
	Background(lipgloss.Color("#2ea043")).
	Foreground(t.Foreground)

// MenuTitleStyle paints the popup header: an orange title that stands apart
// from the green accent of the selected row and from the muted body text.
var MenuTitleStyle = lipgloss.NewStyle().
	Background(t.Surface).
	Foreground(lipgloss.Color("#f0883e"))

// HelpHintActiveStyle paints a help-bar key as an engaged-action pill, reusing
// the active-tab green so it reads as "this action is currently on". It has no
// extra padding or bold so the hint row keeps its exact width budget.
var HelpHintActiveStyle = lipgloss.NewStyle().
	Background(lipgloss.Color("#2ea043")).
	Foreground(t.Background)

var (
	BaseStyle = lipgloss.NewStyle().
			Background(t.Background).
			Foreground(t.Foreground)

	MainPanelStyle = lipgloss.NewStyle().
			Background(t.Background).
			Padding(0, 0)

	HelpBarStyle = lipgloss.NewStyle().
			Background(t.Surface).
			Foreground(t.Muted).
			Padding(0, 1).
			Height(1)

	TabActiveStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("#2ea043")).
			Foreground(t.Background).
			Bold(true).
			Padding(0, 1)

	TabInactiveStyle = lipgloss.NewStyle().
				Background(t.Background).
				Foreground(t.Muted).
				Padding(0, 1)

	SubTabActiveStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("#2ea043")).
				Foreground(t.Background).
				Bold(true).
				Padding(0, 1)

	SubTabInactiveStyle = lipgloss.NewStyle().
				Background(t.Background).
				Foreground(t.Muted).
				Padding(0, 1)
)

func Truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// cutPlain trims s (ANSI-stripped) to at most n visible runes, dropping any
// trailing ANSI sequences. Assembled content rows are clamped to the pane
// width with it so the viewport never word-wraps a row into continuation
// lines that it then completes with unstyled whitespace.
func cutPlain(s string, n int) string {
	if n < 0 {
		n = 0
	}
	r := []rune(ansiStripped(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n])
}

// ansiStripped removes SGR escape sequences from a string, leaving the
// visible text behind.
func ansiStripped(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		if inEsc {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
			continue
		}
		if r == '\x1b' {
			inEsc = true
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
