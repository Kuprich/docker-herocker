package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
)

// statusPickerLabel builds the checkbox row for a docker container state.
// checked marks the state as part of the active status filter.
func statusPickerLabel(state string, checked bool) string {
	box := "[ ]"
	if checked {
		box = "[x]"
	}
	return box + " " + state
}

// statusSelected reports whether state is part of the active status filter.
func (m Model) statusSelected(state string) bool {
	for _, s := range m.statusFilter {
		if s == state {
			return true
		}
	}
	return false
}

// statusPickerMeasure returns the box size of the status picker window. The
// box is centered on the screen just like the context menu, so the size only
// depends on the fixed row set.
func statusPickerMeasure() (w, h int) {
	header := "Filter by status"
	w = 2 // left + right border cells
	for _, st := range containerStatuses {
		w = max(w, runewidth.StringWidth(statusPickerLabel(st, true)))
	}
	for _, l := range []string{"Select all", "Clear"} {
		w = max(w, runewidth.StringWidth(l))
	}
	// the title row reserves a leading and a trailing cell of breathing room
	w = max(w, runewidth.StringWidth(header)+2)
	w = max(w, menuMinWidth) + 3
	h = 2 + 2 + len(containerStatuses) + 1 + 2 // borders + title+separator + statuses + divider + actions
	return w, h
}

// statusBoxAt centers the picker box on the current terminal, mirroring the
// context-menu placement below the tab bar.
func (m Model) statusBoxAt() (x, y, w, h int) {
	w, h = statusPickerMeasure()
	x = max((m.width-w)/2, 0)
	y = max((m.height-h)/2, tabBarHeight+1)
	return x, y, w, h
}

// openStatusPicker shows the picker window and resets its cursor to the top,
// so re-opening always starts from a predictable position.
func (m *Model) openStatusPicker() {
	m.statusOpen = true
	m.statusSel = 0
}

// statusActivate runs the picker row at sel: toggling a state, selecting all
// states, or clearing the filter, then refreshing the current list.
func (m *Model) statusActivate(sel int) tea.Cmd {
	switch {
	case sel < len(containerStatuses):
		m.toggleStatus(containerStatuses[sel])
	case sel == statusSelectAll:
		m.statusFilter = append([]string(nil), containerStatuses...)
	default:
		m.statusFilter = nil
	}
	return m.refreshNow()
}

// toggleStatus flips a single container state in or out of the active filter.
func (m *Model) toggleStatus(state string) {
	for i, s := range m.statusFilter {
		if s == state {
			m.statusFilter = append(m.statusFilter[:i], m.statusFilter[i+1:]...)
			return
		}
	}
	m.statusFilter = append(m.statusFilter, state)
}

// handleStatusKey processes a keystroke while the picker is open. Up/Down move
// the cursor, space/enter toggle the row, Esc closes and Quit falls through so
// ctrl+c/q still exits; everything else drops the window without acting.
func (m Model) handleStatusKey(msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	switch {
	case key.Matches(msg, keys.Up):
		if m.statusSel > 0 {
			m.statusSel--
		}
		return m, nil, true
	case key.Matches(msg, keys.Down):
		if m.statusSel < statusRowCount-1 {
			m.statusSel++
		}
		return m, nil, true
	case key.Matches(msg, keys.StartStop):
		return m, m.statusActivate(m.statusSel), true
	case msg.String() == "enter":
		return m, m.statusActivate(m.statusSel), true
	case key.Matches(msg, keys.Back):
		m.statusOpen = false
		return m, nil, true
	case key.Matches(msg, keys.Quit):
		return m, nil, false
	default:
		m.statusOpen = false
		return m, nil, true
	}
}

// handleStatusMouse processes a mouse event while the picker is open. A left
// press inside the box toggles that row right away; releases and motions never
// close it; any other press or the wheel drops the window.
func (m Model) handleStatusMouse(msg tea.MouseMsg) (Model, tea.Cmd) {
	if msg.Type == tea.MouseLeft && msg.Action != tea.MouseActionMotion {
		x, y, w, h := m.statusBoxAt()
		sx, sy := msg.X-1, msg.Y-1 // mouse coords are 1-based screen cells
		if sx >= x && sx < x+w && sy >= y && sy < y+h {
			row := sy - y - 1 // below the top border
			row -= 2          // title row + the horizontal separator below it
			if idx, ok := menuRowItem(statusRowCount, []int{len(containerStatuses)}, row); ok {
				m.statusSel = idx
				return m, m.statusActivate(idx)
			}
			return m, nil
		}
	}
	if msg.Action == tea.MouseActionRelease || msg.Action == tea.MouseActionMotion {
		return m, nil
	}
	m.statusOpen = false
	return m, nil
}

// renderStatusPicker draws the picker box as one string per screen row, each
// exactly w cells wide, mirroring renderContainerMenu: the overlay blocks the
// app content below, and every segment carries a surface/selection background
// so no terminal cell can leak through.
func (m Model) renderStatusPicker() []string {
	w, _ := statusPickerMeasure()
	iw := max(w-2, 0)
	rows := []string{MenuBoxStyle.Render("┌" + strings.Repeat("─", iw) + "┐")}
	rows = append(rows,
		MenuBoxStyle.Render("│ ")+
			MenuTitleStyle.Render(centerMenuRunes("Filter by status", max(iw-2, 0)))+
			MenuBoxStyle.Render(" │"))
	rows = append(rows, MenuBoxStyle.Render("│"+strings.Repeat("─", iw)+"│"))
	for i := 0; i < statusRowCount; i++ {
		style := MenuItemStyle
		if i == m.statusSel {
			style = MenuActiveItemStyle
		}
		var label string
		switch {
		case i < len(containerStatuses):
			label = statusPickerLabel(containerStatuses[i], m.statusSelected(containerStatuses[i]))
		case i == statusSelectAll:
			label = "Select all"
		default:
			label = "Clear"
		}
		if menuHasDivider([]int{len(containerStatuses)}, i) {
			rows = append(rows, MenuBoxStyle.Render("│"+strings.Repeat("─", iw)+"│"))
		}
		rows = append(rows, MenuBoxStyle.Render("│")+
			style.Render(" "+padMenuRunes(label, max(iw-1, 0)))+
			MenuBoxStyle.Render("│"))
	}
	rows = append(rows, MenuBoxStyle.Render("└"+strings.Repeat("─", iw)+"┘"))
	return rows
}

// spliceStatusPicker overlays the picker box onto the fully rendered frame,
// one row per screen row, in place. It never adds rows, preserving the View()
// height and mouse→buffer coordinate invariants.
func (m Model) spliceStatusPicker(content string) string {
	x, y, w, _ := m.statusBoxAt()
	lines := strings.Split(content, "\n")
	for r, pr := range m.renderStatusPicker() {
		sy := y + r
		if sy < 0 || sy >= len(lines) {
			continue
		}
		lines[sy] = insertStyledLine(lines[sy], x, w, pr)
	}
	return strings.Join(lines, "\n")
}
