package tui

import "github.com/charmbracelet/lipgloss"

const (
	sidebarWidth     = 22
	statusBarHeight  = 1
	helpBarHeight    = 1
	logRefreshRate   = 200
)

var errorStyle = lipgloss.NewStyle().
	Foreground(t.Error).
	Bold(true).
	Padding(2, 2)