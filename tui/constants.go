package tui

import (
	"time"

	"github.com/charmbracelet/lipgloss"
)

const (
	tabBarHeight    = 3
	subTabBarHeight = 3
	helpBarHeight   = 1
	// splitRatio is the fraction of the pane height given to the top (list)
	// pane; the bottom Info/Logs submenu gets the remainder (70%).
	splitRatio = 0.30

	// refreshInterval is the period of the single global list auto-refresh
	// ticker.
	refreshInterval = 2 * time.Second

	// logRefreshInterval drives a dedicated fast ticker for the Logs pane
	// so log lines appear almost live while lists keep their slow cadence.
	logRefreshInterval = 500 * time.Millisecond

	// logResyncGap is the maximum age of the incremental-fetch cursor. If
	// the last log fetch was older (user left the pane), a full tail reload
	// happens instead of a potentially huge since-based catch-up.
	logResyncGap = 5 * time.Second

	// dragTimeout finalizes an in-flight text drag when no mouse event has
	// arrived for its duration. The tick is re-armed on every press/motion,
	// so only a genuinely lost release (drag ending outside the terminal,
	// focus loss) trips it. A timed-out drag becomes a normal selection but
	// is never auto-copied.
	dragTimeout = 2 * time.Second

	// copyToastDuration is how long the "Copied to clipboard" badge stays in
	// the top-right corner after a successful OSC 52 copy. It mirrors
	// dragTimeout so the toast and the lost-release timeout expire together.
	copyToastDuration = 2 * time.Second

	// logMaxWrappedLines caps the pre-wrapped Logs pane buffer so
	// incremental appends cannot grow it without bound.
	logMaxWrappedLines = 4000

	// appMarginX is the number of empty background columns between the
	// terminal edges and ALL application content. Every width-sensitive
	// renderer must use innerW() instead of m.width, and mouse click
	// hit-tests must subtract the left margin from the x coordinate.
	appMarginX = 1
)

// Column boundaries of the container list row layout:
// " %s  %-29s %-11s  %7s   %13s   %-20s  %-22s"
const (
	stateCol = 34 // 1 space + dot + 2 spaces + name(29) + gap
	cpuCol   = 47 // state(11) + 2 gaps
	memCol   = 57 // cpu(7) + 3 gaps
	imageCol = 73 // mem(13) + 3 gaps
	portsCol = 95 // image(20) + 2 gaps
)

// memUsageColor highlights the used-portion of the MEM column, matching the
// published-host-port highlight so it reads as the same "live" metric.
const memUsageColor = lipgloss.Color("#f5a742")

var errorStyle = lipgloss.NewStyle().
	Foreground(t.Error).
	Bold(true).
	Padding(2, 2)
