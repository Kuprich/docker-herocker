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
