package tui

import (
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/kuri4/dockerherocker/docker"
	"github.com/mattn/go-runewidth"
)

// menuItem is a single action in the container context menu. activate
// returns the tea.Cmd to dispatch (nil keeps the popup open). confirm marks
// a staging item (Remove variants) whose activation swaps the popup into the
// destructive-action confirm stage instead of dispatching anything;
// removeVolumes selects the variant that removes the container with its data.
type menuItem struct {
	label         string
	confirm       bool
	removeVolumes bool // pairs with confirm: "Remove with data" variant
	activate      func() tea.Cmd
}

// popupMenu is the container context menu, centered on the screen. x,y holds
// the 0-based cell of its top-left corner; w,h its box size in cells (already
// clamped to the terminal). confirm marks the second, destructive stage.
type popupMenu struct {
	items   []menuItem
	x, y    int
	w, h    int
	sel     int
	confirm bool
	header  string
}

// menuMinWidth is the minimum content width (cells inside the borders) of the
// popup, so a short two-item menu does not hug its labels; the confirm-stage
// header widens the box further whenever it is longer.
const menuMinWidth = 16

// containerMenuItems builds the first-stage actions for a container: a
// state-dependent Start/Stop plus the (confirmed) Remove and Remove-with-data
// variants.
func (m Model) containerMenuItems(c docker.Container) []menuItem {
	label := "Start"
	if c.State == "running" {
		label = "Stop"
	}
	return []menuItem{
		{label: label, activate: m.toggleContainer},
		{label: "Remove", confirm: true},
		{label: "Remove with data", confirm: true, removeVolumes: true},
	}
}

// enterRemoveConfirm swaps the popup into the destructive-action stage: a
// header naming the target plus Yes/No items. withData selects the variant
// that also removes the container's volumes. The selection resets to Yes so
// a deliberate second Enter executes; Esc still cancels.
func (m *Model) enterRemoveConfirm(withData bool) {
	c := m.containers[m.selectedIdx]
	name := containerDisplayName(c)
	m.menu.confirm = true
	m.menu.sel = 0
	m.menu.header = "Remove " + name + "?"
	if withData {
		m.menu.header = "Remove " + name + " and its volumes?"
	}
	remove := m.removeContainer
	if withData {
		remove = m.removeContainerVolumes
	}
	m.menu.items = []menuItem{
		{label: "Yes, remove", activate: remove},
		{label: "No, cancel", activate: func() tea.Cmd { return nil }},
	}
	m.menu.w, m.menu.h = menuMeasure(m.menu.items, m.menu.header)
	// keep the enlarged confirm box centered like the first stage
	m.menu.x = max((m.width-m.menu.w)/2, 0)
	m.menu.y = max((m.height-m.menu.h)/2, tabBarHeight+1)
}

// removeContainer / removeContainerVolumes remove the selected container
// (with its volumes) and refresh the list, mirroring toggleContainer and
// restartContainer.
func (m Model) removeContainer() tea.Cmd        { return m.removeContainerCmd(false) }
func (m Model) removeContainerVolumes() tea.Cmd { return m.removeContainerCmd(true) }

func (m Model) removeContainerCmd(volumes bool) tea.Cmd {
	if m.activeTab != tabContainers || m.selectedIdx >= len(m.containers) {
		return nil
	}
	c := m.containers[m.selectedIdx]
	return func() tea.Msg {
		var err error
		if volumes {
			err = m.docker.RemoveContainerVolumes(c.ID)
		} else {
			err = m.docker.RemoveContainer(c.ID)
		}
		if err != nil {
			return errMsg{err}
		}
		time.Sleep(500 * time.Millisecond)
		return m.refreshNow()()
	}
}

// containerDisplayName returns the leading (dash-stripped) container name,
// falling back to the raw ID when the container has none.
func containerDisplayName(c docker.Container) string {
	for _, n := range c.Names {
		if n != "" {
			return strings.TrimPrefix(n, "/")
		}
	}
	return c.ID
}

// buildContainerMenu builds the popup for the selected container, centered on
// the screen and clamped into the terminal, keeping it clear of the tab bar.
// The keyboard's x opens it; there is no mouse anchor anymore.
func (m Model) buildContainerMenu() popupMenu {
	c := m.containers[m.selectedIdx]
	items := m.containerMenuItems(c)
	header := "Действия с контейнером " + containerDisplayName(c)
	w, h := menuMeasure(items, header)
	return popupMenu{
		items:  items,
		header: header,
		x:      max((m.width-w)/2, 0),
		y:      max((m.height-h)/2, tabBarHeight+1),
		w:      w,
		h:      h,
	}
}

// menuMeasure computes the popup box size: 2 border columns plus the widest
// label (or header), never narrower than menuMinWidth, plus one row per item
// plus two border rows (and the optional header row).
func menuMeasure(items []menuItem, header string) (w, h int) {
	w = 2 // left + right border cells
	for _, it := range items {
		if l := runewidth.StringWidth(it.label); l > w {
			w = l
		}
	}
	if header != "" {
		if l := runewidth.StringWidth(header); l > w {
			w = l
		}
	}
	w = max(w, menuMinWidth)
	w += 3 // inside padding: one leading indent cell per text row + filler
	h = len(items) + 2
	if header != "" {
		h++
	}
	return w, h
}

// renderContainerMenu draws the popup box as one string per screen row, each
// exactly w cells wide and carrying its own surface/selection background, so
// the overlay can block what the app drew underneath. Text rows are indented
// by one leading space so labels do not touch the border.
func (m Model) renderContainerMenu() []string {
	iw := max(m.menu.w-2, 0)
	rows := []string{MenuBoxStyle.Render("┌" + strings.Repeat("─", iw) + "┐")}
	if m.menu.header != "" {
		rows = append(rows, MenuBoxStyle.Render("│"+padMenuRunes(" "+m.menu.header, iw)+"│"))
	}
	for i, it := range m.menu.items {
		style := MenuItemStyle
		if i == m.menu.sel {
			style = MenuActiveItemStyle
		}
		rows = append(rows, MenuBoxStyle.Render("│"+style.Render(" "+padMenuRunes(it.label, iw-1))+"│"))
	}
	rows = append(rows, MenuBoxStyle.Render("└"+strings.Repeat("─", iw)+"┘"))
	return rows
}

// padMenuRunes right-pads visible text to exactly n display cells, trimming
// with an ellipsis (fitRunes) when it is too wide.
func padMenuRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if w := runewidth.StringWidth(s); w > n {
		return fitRunes(s, n)
	}
	return s + strings.Repeat(" ", n-runewidth.StringWidth(s))
}

// menuActivate invokes the item at index i. A command means the popup closes
// and the command is dispatched (Start/Stop, "Yes, remove"); "No, cancel"
// returns nil with the popup still open and is closed here. A staging item
// (Remove variants) swaps the popup into its confirm stage and stays open.
func (m *Model) menuActivate(i int) tea.Cmd {
	if i < 0 || i >= len(m.menu.items) {
		m.menuOpen = false
		return nil
	}
	if m.menu.items[i].confirm {
		m.enterRemoveConfirm(m.menu.items[i].removeVolumes)
		return nil
	}
	wasConfirm := m.menu.confirm
	cmd := m.menu.items[i].activate()
	switch {
	case cmd != nil:
		m.menuOpen = false
		return cmd
	case wasConfirm && m.menu.confirm:
		// "No, cancel": nothing to dispatch, drop the popup.
		m.menuOpen = false
		return nil
	default:
		// No-op action; leave the popup as it is.
		return nil
	}
}

// handleMenuKey processes a keystroke while the popup is open. handled=false
// (Quit only) lets the caller's normal key handling proceed so ctrl+c/q still
// quits the app; every other key closes the menu and is swallowed.
func (m Model) handleMenuKey(msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	switch {
	case key.Matches(msg, keys.Up):
		if m.menu.sel > 0 {
			m.menu.sel--
		}
		return m, nil, true
	case key.Matches(msg, keys.Down):
		if m.menu.sel < len(m.menu.items)-1 {
			m.menu.sel++
		}
		return m, nil, true
	case msg.String() == "enter":
		return m, m.menuActivate(m.menu.sel), true
	case key.Matches(msg, keys.Back):
		m.menuOpen = false
		return m, nil, true
	case key.Matches(msg, keys.Quit):
		return m, nil, false
	default:
		m.menuOpen = false
		return m, nil, true
	}
}

// handleMenuMouse processes a mouse event while the popup is open. A left
// press inside the box acts on that item right away; releases and motions
// never close it (the press that opened it delivers a release right after);
// any other press or the wheel drops the popup.
func (m Model) handleMenuMouse(msg tea.MouseMsg) (Model, tea.Cmd) {
	if msg.Type == tea.MouseLeft && msg.Action != tea.MouseActionMotion {
		sx, sy := msg.X-1, msg.Y-1 // mouse coords are 1-based screen cells
		if sx >= m.menu.x && sx < m.menu.x+m.menu.w &&
			sy >= m.menu.y && sy < m.menu.y+m.menu.h {
			item := sy - m.menu.y - 1 // below the top border
			if m.menu.header != "" {
				item--
			}
			if item >= 0 && item < len(m.menu.items) {
				m.menu.sel = item
				return m, m.menuActivate(item)
			}
			return m, nil
		}
	}
	if msg.Action == tea.MouseActionRelease || msg.Action == tea.MouseActionMotion {
		return m, nil
	}
	m.menuOpen = false
	return m, nil
}

// splicePopup overlays the popup box onto the fully rendered frame, one menu
// row per target screen row, in place. It never adds rows, so the View()
// height and mouse→buffer coordinate invariants stay untouched.
func (m Model) splicePopup(content string) string {
	lines := strings.Split(content, "\n")
	for r, pr := range m.renderContainerMenu() {
		sy := m.menu.y + r
		if sy < 0 || sy >= len(lines) {
			continue
		}
		lines[sy] = insertStyledLine(lines[sy], m.menu.x, m.menu.w, pr)
	}
	return strings.Join(lines, "\n")
}

// insertStyledLine overlays one popup row (exactly cover cells wide) onto a
// styled content line at visible column col. Escape sequences are hopped over
// while visible cells are counted; sequences whose span falls inside the
// covered region are dropped, and the tail right of the block is re-painted
// with the base background (repaintBare) so no default-terminal cells leak.
func insertStyledLine(line string, col, cover int, popup string) string {
	base := lipgloss.NewStyle().Background(t.Background).Foreground(t.Foreground)
	var before, after []rune
	part := 0 // 0 before / 1 covered / 2 after
	vis := 0
	i := 0
	in := []rune(line)
	for i < len(in) {
		if in[i] == '\x1b' {
			j := i + 1
			for j < len(in) {
				if c := in[j]; (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
					break
				}
				j++
			}
			if j >= len(in) {
				break
			}
			switch part {
			case 0:
				before = append(before, in[i:j+1]...)
			case 2:
				after = append(after, in[i:j+1]...)
			}
			i = j + 1
			continue
		}
		if part == 0 && vis == col {
			part = 1
		}
		switch part {
		case 0:
			before = append(before, in[i])
		case 2:
			after = append(after, in[i])
		}
		vis++
		i++
		if part == 1 && vis == col+cover {
			part = 2
		}
	}
	if part == 0 {
		// Line shorter than the insertion column: extend it with blank
		// background cells so the popup block still lands at col.
		return line + base.Render(strings.Repeat(" ", col-vis)) + popup
	}
	return string(before) + popup + repaintBare(after, base)
}
