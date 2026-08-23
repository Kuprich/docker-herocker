package tui

import (
	"github.com/charmbracelet/lipgloss"
	"github.com/kuri4/dockerherocker/config"
)

var t = config.DefaultTheme()

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
		Background(t.Surface).
		Foreground(t.Accent).
		Bold(true).
		Padding(0, 1)

	TabInactiveStyle = lipgloss.NewStyle().
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