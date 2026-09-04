package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/kuri4/dockerherocker/docker"
)

// stateColor maps a container state to its theme color.
func stateColor(state string) lipgloss.Color {
	switch state {
	case "running":
		return t.Success
	case "paused":
		return t.Warning
	case "restarting", "dead":
		return t.Error
	case "exited":
		return t.Muted
	default: // created, removing, ...
		return t.Info
	}
}

// imageStatus classifies an image by whether any container uses it and
// whether it still carries a tag, mirroring the container table styling.
type imageStatus struct {
	label string // IN-USE / UNUSED / DANGLING
	dot   string // ● / ○
	color lipgloss.Color
}

// classifyImage maps an image to its status. Containers>0 means at least one
// container is using it; -1 (unknown) is treated as 0. An image with no tags
// is dangling (a prune candidate).
func classifyImage(containers int64, ntags int) imageStatus {
	if containers > 0 {
		return imageStatus{"IN-USE", "●", t.Success}
	}
	if ntags == 0 {
		return imageStatus{"DANGLING", "○", t.Warning}
	}
	return imageStatus{"UNUSED", "●", t.Muted}
}

// sizeUnitColor paints the size unit uniformly gray so it does not compete
// with the numeric value itself.
func sizeUnitColor(unit string) lipgloss.Color {
	return t.Muted
}

// colorize wraps value in color and pads the remainder to maxW with the
// theme background - an ANSI reset inside the styled value would otherwise
// leave the rest of the line with the default terminal background.
// Values longer than maxW are returned plainly truncated.
func colorize(value string, maxW int, color lipgloss.Color) string {
	if len([]rune(value)) > maxW {
		return Truncate(value, maxW)
	}
	pad := maxW - len([]rune(value))
	return lipgloss.NewStyle().Foreground(color).Render(value) +
		lipgloss.NewStyle().Background(t.Background).Render(strings.Repeat(" ", pad))
}

// renderPortsCell renders the ports list as a self-contained cell that
// fills exactly width columns: every segment (including separators and
// padding) carries the given background, so no part of the line falls back
// to the default terminal background. ok=false when the plain form exceeds
// width - the caller should fall back to a plain truncated rendering.
func renderPortsCell(ports []docker.Port, width int, bg lipgloss.Color) (string, bool) {
	plain := formatPorts(ports)
	if len([]rune(plain)) > width {
		return "", false
	}
	base := lipgloss.NewStyle().Background(bg).Foreground(t.Foreground)
	fill := lipgloss.NewStyle().Background(bg)
	// hostStyle draws the published (host-side) port, the value bound on the
	// machine running Docker rather than inside the container, so the port a
	// user would actually connect to stands out from the mapping.
	hostStyle := lipgloss.NewStyle().Background(bg).Foreground(lipgloss.Color("#f5a742"))
	suffix := func(proto string) lipgloss.Style {
		if proto == "udp" {
			return lipgloss.NewStyle().Background(bg).Foreground(t.Warning)
		}
		return lipgloss.NewStyle().Background(bg).Foreground(t.Muted)
	}

	var b strings.Builder
	for i, p := range ports {
		if i > 0 {
			b.WriteString(base.Render(", "))
		}
		if p.PublicPort != 0 {
			b.WriteString(hostStyle.Render(fmt.Sprintf("%d", p.PublicPort)))
			b.WriteString(base.Render(fmt.Sprintf("->%d", p.PrivatePort)))
		} else {
			b.WriteString(base.Render(fmt.Sprintf("%d", p.PrivatePort)))
		}
		b.WriteString(suffix(p.Type).Render("/" + p.Type))
	}
	pad := width - len([]rune(plain))
	if pad > 0 {
		b.WriteString(fill.Render(strings.Repeat(" ", pad)))
	}
	return b.String(), true
}
