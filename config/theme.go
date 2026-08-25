package config

import "github.com/charmbracelet/lipgloss"

type Theme struct {
	Background lipgloss.Color
	Foreground lipgloss.Color
	Accent     lipgloss.Color
	AccentDim  lipgloss.Color
	Success    lipgloss.Color
	Warning    lipgloss.Color
	Error      lipgloss.Color
	Muted      lipgloss.Color
	Surface    lipgloss.Color
	Border     lipgloss.Color
	Info       lipgloss.Color
}

func DefaultTheme() Theme {
	return Theme{
		Background: lipgloss.Color("#0d1117"),
		Foreground: lipgloss.Color("#e6edf3"),
		Accent:     lipgloss.Color("#3fb950"),
		AccentDim:  lipgloss.Color("#2ea043"),
		Success:    lipgloss.Color("#3fb950"),
		Warning:    lipgloss.Color("#d29922"),
		Error:      lipgloss.Color("#f85149"),
		Muted:      lipgloss.Color("#8b949e"),
		Surface:    lipgloss.Color("#161b22"),
		Border:     lipgloss.Color("#30363d"),
		Info:       lipgloss.Color("#58a6ff"),
	}
}
