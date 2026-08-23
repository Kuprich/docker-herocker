package tui

import (
	"github.com/charmbracelet/lipgloss"
	"github.com/kuri4/dockerherocker/config"
)

var t = config.DefaultTheme()

var (
	AppStyle = lipgloss.NewStyle().
		Background(t.Background).
		Height(1)

	BaseStyle = lipgloss.NewStyle().
		Background(t.Background).
		Foreground(t.Foreground)

	SidebarStyle = lipgloss.NewStyle().
		Background(t.Surface).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.Border).
		Width(20)

	SidebarActiveStyle = SidebarStyle.Copy().
		BorderForeground(t.Accent)

	MainPanelStyle = lipgloss.NewStyle().
		Background(t.Background).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.Border).
		Padding(1, 1).
		MarginLeft(1)

	MainPanelActiveStyle = MainPanelStyle.Copy().
		BorderForeground(t.Accent)

	StatusBarStyle = lipgloss.NewStyle().
		Background(lipgloss.Color("#21262d")).
		Foreground(t.Foreground).
		Padding(0, 1).
		Height(1)

	HelpBarStyle = lipgloss.NewStyle().
		Background(t.Surface).
		Foreground(t.Muted).
		Padding(0, 1).
		Height(1)

	StatusDotRunning = lipgloss.NewStyle().
			Foreground(t.Success).
			SetString("●").
			String()

	StatusDotStopped = lipgloss.NewStyle().
			Foreground(t.Error).
			SetString("●").
			String()

	StatusDotPaused = lipgloss.NewStyle().
			Foreground(t.Warning).
			SetString("●").
			String()

	StatusDotExited = lipgloss.NewStyle().
			Foreground(t.Muted).
			SetString("○").
			String()

SelectedRow = lipgloss.NewStyle().
		Background(lipgloss.Color("#1c2d1f")).
		Foreground(t.Foreground).
		Padding(0, 1)

	TableHeader = lipgloss.NewStyle().
			Foreground(t.Accent).
			Bold(true).
			Padding(0, 1)

	TableRow = lipgloss.NewStyle().
			Padding(0, 1)

	SidebarItem = lipgloss.NewStyle().
			Padding(0, 1).
			Width(18)

SidebarItemActive = SidebarItem.Copy().
		Background(lipgloss.Color("#1c2d1f")).
		Foreground(t.Accent).
		Bold(true)

	Divider = lipgloss.NewStyle().
		Foreground(t.Muted).
		SetString("│").
		String()
)

func StatusDot(state string) string {
	switch state {
	case "running":
		return StatusDotRunning
	case "paused":
		return StatusDotPaused
	case "exited":
		return StatusDotExited
	default:
		return StatusDotStopped
	}
}

func Truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}