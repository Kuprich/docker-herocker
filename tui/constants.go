package tui

import "github.com/charmbracelet/lipgloss"

const (
	tabBarHeight    = 3
	subTabBarHeight = 3
	helpBarHeight   = 1
	logRefreshRate  = 200
	splitRatio      = 0.50

	// appMarginX is the number of empty background columns between the
	// terminal edges and ALL application content. Every width-sensitive
	// renderer must use innerW() instead of m.width, and mouse click
	// hit-tests must subtract the left margin from the x coordinate.
	appMarginX = 1
)

var errorStyle = lipgloss.NewStyle().
	Foreground(t.Error).
	Bold(true).
	Padding(2, 2)
