package tui

import "github.com/charmbracelet/lipgloss"

const (
	tabBarHeight    = 3
	subTabBarHeight = 1
	helpBarHeight   = 1
	logRefreshRate  = 200
	splitRatio      = 0.50
)

var errorStyle = lipgloss.NewStyle().
	Foreground(t.Error).
	Bold(true).
	Padding(2, 2)