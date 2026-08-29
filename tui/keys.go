package tui

import "github.com/charmbracelet/bubbles/key"

type keyMap struct {
	Up        key.Binding
	Down      key.Binding
	Back      key.Binding
	Quit      key.Binding
	Help      key.Binding
	ToggleAll key.Binding
	StartStop key.Binding
	Restart   key.Binding
	Follow    key.Binding
	Copy      key.Binding
	One       key.Binding
	Two       key.Binding
	Three     key.Binding
	Four      key.Binding
	Select    key.Binding
}

var keys = keyMap{
	Up: key.NewBinding(
		key.WithKeys("up", "k"),
		key.WithHelp("↑/k", "move up"),
	),
	Down: key.NewBinding(
		key.WithKeys("down", "j"),
		key.WithHelp("↓/j", "move down"),
	),
	Back: key.NewBinding(
		key.WithKeys("esc"),
		key.WithHelp("esc", "back"),
	),
	Quit: key.NewBinding(
		key.WithKeys("ctrl+c", "q"),
		key.WithHelp("ctrl+c/q", "quit"),
	),
	Help: key.NewBinding(
		key.WithKeys("?"),
		key.WithHelp("?", "toggle help"),
	),
	ToggleAll: key.NewBinding(
		key.WithKeys("a"),
		key.WithHelp("a", "show all/active"),
	),
	StartStop: key.NewBinding(
		key.WithKeys(" "),
		key.WithHelp("space", "start/stop"),
	),
	Restart: key.NewBinding(
		key.WithKeys("r"),
		key.WithHelp("r", "restart"),
	),
	Follow: key.NewBinding(
		key.WithKeys("f"),
		key.WithHelp("f", "follow logs"),
	),
	Copy: key.NewBinding(
		key.WithKeys("y"),
		key.WithHelp("y", "copy selection"),
	),
	One: key.NewBinding(
		key.WithKeys("1"),
		key.WithHelp("1", "containers"),
	),
	Two: key.NewBinding(
		key.WithKeys("2"),
		key.WithHelp("2", "images"),
	),
	Three: key.NewBinding(
		key.WithKeys("3"),
		key.WithHelp("3", "volumes"),
	),
	Four: key.NewBinding(
		key.WithKeys("4"),
		key.WithHelp("4", "networks"),
	),
	Select: key.NewBinding(
		key.WithKeys(""),
		key.WithHelp("Shift+drag", "native text selection"),
	),
}

func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Up, k.Down, k.Back, k.Quit, k.Help}
}

func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Up, k.Down, k.Follow, k.Copy, k.Select},
		{k.StartStop, k.Restart, k.ToggleAll},
		{k.Back, k.Help, k.Quit},
	}
}
