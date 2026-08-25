package tui

import (
	"time"

	"github.com/charmbracelet/lipgloss"
)

const (
	tabBarHeight    = 3
	subTabBarHeight = 3
	helpBarHeight   = 1
	logRefreshRate  = 200
	splitRatio      = 0.50

	// refreshInterval is the period of the single global auto-refresh ticker.
	refreshInterval = 2 * time.Second

	// appMarginX is the number of empty background columns between the
	// terminal edges and ALL application content. Every width-sensitive
	// renderer must use innerW() instead of m.width, and mouse click
	// hit-tests must subtract the left margin from the x coordinate.
	appMarginX = 1
)

// Column boundaries of the container list row layout:
// " %s  %-29s %-11s  %-32s  %-34s"
const (
	stateCol = 34 // 1 space + dot + 2 spaces + name(29) + gap
	imageCol = 47 // state(11) + 2 gaps
	portsCol = 81 // image(32) + 2 gaps
)

var errorStyle = lipgloss.NewStyle().
	Foreground(t.Error).
	Bold(true).
	Padding(2, 2)
