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

// colorize wraps value in color when it fits maxW, otherwise returns a
// plainly truncated version - truncating an ANSI-styled string would
// corrupt the escape sequences.
func colorize(value string, maxW int, color lipgloss.Color) string {
	if len([]rune(value)) <= maxW {
		return lipgloss.NewStyle().Foreground(color).Render(value)
	}
	return Truncate(value, maxW)
}

// formatPortsColored renders ports with the protocol suffix highlighted:
// /tcp muted, /udp warning-yellow. The visible characters match
// formatPorts exactly, so both are interchangeable for layout math.
func formatPortsColored(ports []docker.Port) string {
	if len(ports) == 0 {
		return ""
	}
	tcp := lipgloss.NewStyle().Foreground(t.Muted)
	udp := lipgloss.NewStyle().Foreground(t.Warning)
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		var base string
		if p.PublicPort != 0 {
			base = fmt.Sprintf("%d->%d", p.PublicPort, p.PrivatePort)
		} else {
			base = fmt.Sprintf("%d", p.PrivatePort)
		}
		style := tcp
		if p.Type == "udp" {
			style = udp
		}
		parts = append(parts, base+style.Render("/"+p.Type))
	}
	return strings.Join(parts, ", ")
}
