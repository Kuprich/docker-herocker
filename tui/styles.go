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

	SidebarStyle = lipgloss.NewStyle().
		Background(t.Surface).
		Width(20)

	MainPanelStyle = lipgloss.NewStyle().
		Background(t.Background).
		Padding(0, 1)

	HelpBarStyle = lipgloss.NewStyle().
		Background(t.Surface).
		Foreground(t.Muted).
		Padding(0, 1).
		Height(1)

	TableHeader = lipgloss.NewStyle().
		Background(t.Background).
		Foreground(t.Accent).
		Bold(true).
		Padding(0, 1)

	SidebarItem = lipgloss.NewStyle().
		Background(t.Surface).
		Padding(0, 0)

	SidebarItemActive = SidebarItem.Copy().
		Background(lipgloss.Color("#1c2d1f")).
		Foreground(t.Accent).
		Bold(true)
)

func Truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}