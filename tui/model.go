package tui

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/kuri4/dockerherocker/docker"
	"github.com/mattn/go-runewidth"
)

type tab int

const (
	tabContainers tab = iota
	tabImages
	tabVolumes
	tabNetworks
)

type containerMsg []docker.Container
type imageMsg []docker.Image
type volumeMsg []docker.Volume
type networkMsg []docker.Network
type errMsg struct{ err error }

// containerLogMsg carries freshly fetched log lines. incremental reports
// whether content is a since-based append (true) or a full tail replace
// (false), so the handler can distinguish a pending incremental fetch from
// a manual resync.
type containerLogMsg struct {
	id          string
	content     string
	incremental bool
}

// silentErrMsg is a background-fetch failure (logs/details) that must not
// pollute the global error state shown to the user.
type silentErrMsg struct{ err error }
type containerDetailsMsg struct {
	id      string
	details *docker.ContainerDetails
}

type subTab int

const (
	subTabInfo subTab = iota
	subTabLogs
)

// textSel is an app-owned span of text anchored in *buffer* coordinates
// (rows/columns of the pre-wrapped content), so a selection stays put while
// the viewport scrolls or fresh log lines stream in - unlike a terminal's
// own screen-anchored selection.
type textSel struct {
	active     bool
	anR, anC   int // anchor: buffer row + rune column where the drag began
	endR, endC int // current drag endpoint
}

// rowSpan returns the rune-column span [from,to] the selection covers on the
// given buffer row. A to of -1 means the whole row is selected.
func (s textSel) rowSpan(row int) (from, to int, ok bool) {
	top, bot := min(s.anR, s.endR), max(s.anR, s.endR)
	if row < top || row > bot {
		return 0, 0, false
	}
	if top == bot {
		return min(s.anC, s.endC), max(s.anC, s.endC), true
	}
	if row == top {
		col := s.anC
		if s.anR > s.endR {
			col = s.endC
		}
		return col, -1, true
	}
	if row == bot {
		col := s.anC
		if s.anR < s.endR {
			col = s.endC
		}
		return 0, col, true
	}
	return 0, -1, true
}

// isTrivial reports whether the selection covers nothing but the single
// cell where the drag started (i.e. a plain click, which should clear).
func (s textSel) isTrivial() bool {
	return s.anR == s.endR && s.anC == s.endC
}

type Model struct {
	docker *docker.Client

	containers  []docker.Container
	images      []docker.Image
	volumes     []docker.Volume
	networks    []docker.Network
	selectedIdx int
	activeTab   tab
	showAll     bool
	loading     bool
	err         error

	mainViewport viewport.Model
	mainYOff     int
	mainRows     int

	spinner spinner.Model
	help    help.Model
	helpOn  bool
	ready   bool
	width   int
	height  int

	activeSubTab         subTab
	logFollow            bool
	containerLogContent  string
	containerLogViewport viewport.Model
	logSel               textSel
	dragSel              bool
	containerLogID       string
	containerLogLastTS   time.Time
	details              *docker.ContainerDetails
	detailsID            string
	detailViewport       viewport.Model
	detailSel            textSel
	detailDragSel        bool
	dragGen              uint64 // bumped on every drag start/release; guards stale timers
	detailContent        string // plain (ANSI-stripped) Info body, selection buffer geometry
	detailStyled         string // styled Info body, re-decorated per render when selected
}

func New(dcli *docker.Client) Model {
	s := spinner.New()
	s.Style = BaseStyle.Copy().Foreground(t.Accent)
	s.Spinner = spinner.Dot
	return Model{
		docker:      dcli,
		spinner:     s,
		help:        help.New(),
		showAll:     true,
		logFollow:   true,
		selectedIdx: 0,
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.refreshNow(),
		m.loadContainerDetails(),
		refreshTicker(),
		logRefreshTicker(),
		m.spinner.Tick,
	)
}

// refreshTickMsg drives the global list auto-refresh ticker.
type refreshTickMsg struct{}

func refreshTicker() tea.Cmd {
	return tea.Tick(refreshInterval, func(time.Time) tea.Msg { return refreshTickMsg{} })
}

// logRefreshTickMsg is the dedicated fast ticker for the Logs pane.
type logRefreshTickMsg struct{}

func logRefreshTicker() tea.Cmd {
	return tea.Tick(logRefreshInterval, func(time.Time) tea.Msg { return logRefreshTickMsg{} })
}

// dragTimeoutMsg fires when an in-flight drag has gone quiet for dragTimeout;
// its gen field must match m.dragGen or the event belongs to a newer drag.
type dragTimeoutMsg struct{ gen uint64 }

// dragTimer finalizes a stale drag if no further mouse events arrive within
// dragTimeout. Re-arming it on every drag event keeps pauses from tripping it.
func dragTimer(gen uint64) tea.Cmd {
	return tea.Tick(dragTimeout, func(time.Time) tea.Msg { return dragTimeoutMsg{gen: gen} })
}

// innerW returns the content width available to all renderers inside the
// global horizontal margins.
func innerW(terminalW int) int {
	return terminalW - 2*appMarginX
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		cw := innerW(msg.Width)
		m.mainViewport = viewport.New(
			cw,
			msg.Height-tabBarHeight-helpBarHeight-1,
		)
		m.mainViewport.Style = BaseStyle
		m.containerLogViewport = viewport.New(
			cw,
			msg.Height-tabBarHeight-helpBarHeight-subTabBarHeight-4,
		)
		m.containerLogViewport.Style = BaseStyle
		m.detailViewport = viewport.New(
			cw,
			msg.Height-tabBarHeight-helpBarHeight-subTabBarHeight-4,
		)
		m.detailViewport.Style = BaseStyle
		m.fitViewports()
		m.ready = true

	case tea.KeyMsg:
		switch {
		case key.Matches(msg, keys.Quit):
			return m, tea.Quit
		case key.Matches(msg, keys.Help):
			m.helpOn = !m.helpOn
		case msg.String() == "left":
			if m.activeTab == tabContainers {
				m.activeSubTab = subTabInfo
				m.logSel = textSel{}
				m.dragSel = false
				m.detailSel = textSel{}
				m.detailDragSel = false
				return m, m.loadContainerDetails()
			}
		case msg.String() == "right":
			if m.activeTab == tabContainers {
				m.activeSubTab = subTabLogs
				m.detailSel = textSel{}
				m.detailDragSel = false
				return m, m.loadContainerLogs()
			}
		case key.Matches(msg, keys.Up):
			oldIdx := m.selectedIdx
			m.moveUp()
			return m, m.selectionChangedCmds(oldIdx)
		case key.Matches(msg, keys.Down):
			oldIdx := m.selectedIdx
			m.moveDown()
			return m, m.selectionChangedCmds(oldIdx)
		case key.Matches(msg, keys.ToggleAll):
			m.showAll = !m.showAll
			return m, m.refreshNow()
		case key.Matches(msg, keys.StartStop):
			return m, m.toggleContainer()
		case key.Matches(msg, keys.Restart):
			return m, m.restartContainer()
		case key.Matches(msg, keys.Follow):
			if m.activeTab == tabContainers && m.activeSubTab == subTabLogs {
				m.logFollow = !m.logFollow
			}
			return m, nil
		case key.Matches(msg, keys.Copy):
			if m.activeTab == tabContainers {
				switch m.activeSubTab {
				case subTabLogs:
					if m.logSel.active {
						return m, osc52Copy(m.selectionText())
					}
				case subTabInfo:
					if m.detailSel.active {
						return m, osc52Copy(selectedText(m.detailContent, m.detailSel))
					}
				}
			}
		case key.Matches(msg, keys.Back):
			if m.activeTab == tabContainers {
				switch m.activeSubTab {
				case subTabLogs:
					if m.logSel.active || m.dragSel {
						m.logSel = textSel{}
						m.dragSel = false
					} else {
						m.activeSubTab = subTabInfo
						m.detailSel = textSel{}
						m.detailDragSel = false
					}
				case subTabInfo:
					if m.detailSel.active || m.detailDragSel {
						m.detailSel = textSel{}
						m.detailDragSel = false
					} else {
						m.activeSubTab = subTabLogs
						m.logSel = textSel{}
						m.dragSel = false
					}
				}
			}
		case key.Matches(msg, keys.One):
			m.switchTab(tabContainers)
			return m, tea.Batch(m.refreshNow(), m.loadContainerDetails())
		case key.Matches(msg, keys.Two):
			m.switchTab(tabImages)
			return m, m.refreshNow()
		case key.Matches(msg, keys.Three):
			m.switchTab(tabVolumes)
			return m, m.refreshNow()
		case key.Matches(msg, keys.Four):
			m.switchTab(tabNetworks)
			return m, m.refreshNow()
		}

	case refreshTickMsg:
		return m, tea.Batch(m.refreshNow(), refreshTicker())

	case logRefreshTickMsg:
		var cmd tea.Cmd = logRefreshTicker()
		if m.activeTab == tabContainers && m.activeSubTab == subTabLogs {
			cmd = tea.Batch(cmd, m.autoRefreshLogs())
		}
		return m, cmd

	case dragTimeoutMsg:
		// A stale timer from an old drag must never finalize a newer one.
		if msg.gen != m.dragGen {
			return m, nil
		}
		// A release was lost (drag ended outside the terminal, focus loss):
		// snap the in-flight drag into a final selection. Deliberately no
		// auto-copy — the user never completed the gesture.
		switch {
		case m.dragSel:
			m.dragSel = false
			m.logSel.active = !m.logSel.isTrivial()
		case m.detailDragSel:
			m.detailDragSel = false
			m.detailSel.active = !m.detailSel.isTrivial()
		}
		return m, nil

	case containerMsg:
		m.containers = msg
		m.err = nil // successful refresh clears transient errors
		m.loading = false
		if m.selectedIdx >= len(m.containers) {
			m.selectedIdx = 0
		}
		m.fitViewports()
		m.scrollToSelected()
		return m, m.loadContainerDetails()

	case imageMsg:
		m.images = msg
		m.err = nil // successful refresh clears transient errors
		m.loading = false
		if m.selectedIdx >= len(m.images) {
			m.selectedIdx = 0
		}
		m.fitViewports()
		m.scrollToSelected()
		return m, nil

	case volumeMsg:
		m.volumes = msg
		m.err = nil // successful refresh clears transient errors
		m.loading = false
		if m.selectedIdx >= len(m.volumes) {
			m.selectedIdx = 0
		}
		m.fitViewports()
		m.scrollToSelected()
		return m, nil

	case networkMsg:
		m.networks = msg
		m.err = nil // successful refresh clears transient errors
		m.loading = false
		if m.selectedIdx >= len(m.networks) {
			m.selectedIdx = 0
		}
		m.fitViewports()
		m.scrollToSelected()
		return m, nil

	case containerLogMsg:
		// Drop fetches for a container we are no longer viewing (races
		// between selection changes and in-flight requests).
		if m.selectedIdx >= len(m.containers) || m.containers[m.selectedIdx].ID != msg.id {
			return m, nil
		}
		// Store pre-wrapped text so the persistent viewport's line count
		// matches what the pane displays - otherwise AtBottom/GotoBottom
		// anchor to the wrong offsets and fresh lines pile up off-screen.
		// Follow the tail always when the checkbox is on; otherwise only
		// when the user is already at the bottom. A live selection (or an
		// in-flight drag) freezes the view so new lines can't scroll the
		// highlighted rows off-screen; follow resumes once it is cleared.
		follow := (m.logFollow || m.containerLogViewport.AtBottom()) && !m.logSel.active && !m.dragSel
		wrapped := wrapLogCells(msg.content, innerW(m.width)-1)
		if msg.incremental {
			if wrapped != "" {
				if m.containerLogContent != "" {
					m.containerLogContent += "\n"
				}
				m.containerLogContent += wrapped
				if lines := strings.Count(m.containerLogContent, "\n") + 1; lines > 2*logMaxWrappedLines {
					m.containerLogContent = pruneLines(m.containerLogContent, logMaxWrappedLines)
					// pruning drops the oldest lines, so buffer-anchored
					// selection rows would point at the wrong text
					m.logSel = textSel{}
					m.dragSel = false
				}
			}
		} else {
			// A full replace often just re-issues the same tail after a quiet
			// period (an exited/quiet container gets a full reload on every
			// tick once its cursor goes stale). If the content is byte-for-byte
			// unchanged, the view rows keep their identity, so an active
			// selection AND an in-flight drag must be left untouched. Only a
			// genuinely different buffer re-maps the selection by text and
			// cancels a mid-flight drag (container recreated, log rotated).
			if m.containerLogContent != wrapped {
				oldContent, oldSel := m.containerLogContent, m.logSel
				m.containerLogContent = wrapped
				if s, ok := reanchorSelection(oldContent, oldSel, wrapped); ok {
					m.logSel = s
				} else {
					m.logSel = textSel{}
				}
				m.dragSel = false
			}
		}
		m.containerLogID = msg.id
		if ts, ok := containerLogCursor(msg.content); ok {
			m.containerLogLastTS = ts
		}
		m.containerLogViewport.SetContent(m.containerLogContent)
		if follow {
			m.containerLogViewport.GotoBottom()
		}

	case containerDetailsMsg:
		m.details = msg.details
		m.detailsID = msg.id
		m.loading = false
		m.fitDetailViewport()

	case silentErrMsg:
		// background fetch failures (logs/details) are non-fatal; keep
		// previously loaded content

	case errMsg:
		m.err = msg.err
		m.loading = false

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case tea.MouseMsg:
		if msg.Shift || msg.Ctrl || msg.Alt {
			// Let the terminal perform its own native text selection
			// (Shift+drag). Some emulators still forward the drag events to
			// the app, so ignore them instead of jumping/clicking mid-select.
			return m, nil
		}

		// Logs pane: the left button drags an app-owned text selection over
		// the log body (reжим A). It is anchored in buffer coordinates so it
		// tracks correctly through scroll and follow-appends. Sub-tab strip,
		// follow-checkbox and scrollbar presses keep their original handling.
		if m.activeTab == tabContainers && m.activeSubTab == subTabLogs {
			leftDown := msg.Type == tea.MouseLeft && msg.Action != tea.MouseActionMotion
			leftMove := msg.Type == tea.MouseLeft && msg.Action == tea.MouseActionMotion
			up := msg.Action == tea.MouseActionRelease
			if leftDown || leftMove || up {
				// Only left-button presses/motions and releases are ours to
				// consume; wheel and other buttons fall through below so the
				// viewport keeps scrolling.
				if m.dragSel {
					switch {
					case leftDown:
						if r, c, ok := m.logScreenToCell(msg.X, msg.Y, false); ok {
							m.dragGen++
							m.logSel = textSel{anR: r, anC: c, endR: r, endC: c}
							return m, dragTimer(m.dragGen)
						}
					case leftMove:
						if r, c, ok := m.logScreenToCell(msg.X, msg.Y, true); ok {
							m.logSel.endR, m.logSel.endC = r, c
						}
						m.dragGen++
						return m, dragTimer(m.dragGen)
					case up:
						m.dragGen++
						return m, m.finishLogSelection(msg.X, msg.Y)
					}
					return m, nil
				}
				if leftDown {
					if r, c, ok := m.logScreenToCell(msg.X, msg.Y, false); ok {
						m.dragGen++
						m.dragSel = true
						m.logSel = textSel{anR: r, anC: c, endR: r, endC: c}
						return m, dragTimer(m.dragGen)
					}
					// Header, sub-tab strip or scrollbar column: act as a click.
					return m.handleClick(msg.X, msg.Y)
				}
				// Motion / release without an active drag: swallow so a
				// release can never double-fire a press-side click action.
				return m, nil
			}
		}

		// Info pane: the same left-button drag selection, anchored in the plain
		// detail buffer. Only the exact span over a row gets highlighted; the
		// row's original accents around it are preserved (see decorateSelection).
		if m.activeTab == tabContainers && m.activeSubTab == subTabInfo {
			leftDown := msg.Type == tea.MouseLeft && msg.Action != tea.MouseActionMotion
			leftMove := msg.Type == tea.MouseLeft && msg.Action == tea.MouseActionMotion
			up := msg.Action == tea.MouseActionRelease
			if leftDown || leftMove || up {
				if m.detailDragSel {
					switch {
					case leftDown:
						if r, c, ok := m.detailScreenToCell(msg.X, msg.Y, false); ok {
							m.dragGen++
							m.detailSel = textSel{anR: r, anC: c, endR: r, endC: c}
							return m, dragTimer(m.dragGen)
						}
					case leftMove:
						if r, c, ok := m.detailScreenToCell(msg.X, msg.Y, true); ok {
							m.detailSel.endR, m.detailSel.endC = r, c
						}
						m.dragGen++
						return m, dragTimer(m.dragGen)
					case up:
						m.dragGen++
						return m, m.finishDetailSelection(msg.X, msg.Y)
					}
					return m, nil
				}
				if leftDown {
					if r, c, ok := m.detailScreenToCell(msg.X, msg.Y, false); ok {
						m.dragGen++
						m.detailDragSel = true
						m.detailSel = textSel{anR: r, anC: c, endR: r, endC: c}
						return m, dragTimer(m.dragGen)
					}
					return m.handleClick(msg.X, msg.Y)
				}
				return m, nil
			}
		}

		if msg.Type == tea.MouseLeft {
			return m.handleClick(msg.X, msg.Y)
		}

		if m.activeTab == tabContainers && m.mouseInBottomPane(msg.Y) {
			var cmd tea.Cmd
			switch m.activeSubTab {
			case subTabLogs:
				m.containerLogViewport, cmd = m.containerLogViewport.Update(msg)
			case subTabInfo:
				m.detailViewport, cmd = m.detailViewport.Update(msg)
			}
			return m, cmd
		}

		switch msg.Button {
		case tea.MouseButtonWheelUp:
			oldIdx := m.selectedIdx
			m.moveUp()
			return m, m.selectionChangedCmds(oldIdx)
		case tea.MouseButtonWheelDown:
			oldIdx := m.selectedIdx
			m.moveDown()
			return m, m.selectionChangedCmds(oldIdx)
		default:
			m.fitViewports()
			var cmd tea.Cmd
			m.mainViewport, cmd = m.mainViewport.Update(msg)
			m.mainYOff = m.mainViewport.YOffset
			return m, cmd
		}
	}

	if _, isKey := msg.(tea.KeyMsg); !isKey {
		var cmd tea.Cmd
		m.mainViewport, cmd = m.mainViewport.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m Model) View() string {
	if !m.ready {
		return "\n  Initializing…"
	}
	content := lipgloss.JoinVertical(lipgloss.Top,
		m.renderTabBar(),
		m.renderMain(),
		m.renderHelpBar(),
	)
	content = lipgloss.NewStyle().
		Background(t.Background).
		Padding(0, appMarginX).
		Render(content)
	content = lipgloss.Place(m.width, m.height,
		lipgloss.Top, lipgloss.Left,
		content,
		lipgloss.WithWhitespaceBackground(t.Background),
	)
	// View() must not be taller than the terminal: an extra row scrolls the
	// visible content by one and silently shifts mouse→buffer coordinates.
	if lines := strings.Split(content, "\n"); len(lines) > m.height {
		content = strings.Join(lines[:m.height], "\n")
	}
	return content
}

// ---- render helpers ----

func (m Model) renderHelpBar() string {
	cw := innerW(m.width)
	// HelpBarStyle pads 1 col per side, so the wrap budget is cw-2.
	inner := cw - 2
	if inner < 1 {
		inner = 1
	}
	if m.helpOn {
		return HelpBarStyle.Width(cw).Render(fitRunes(m.help.View(keys), inner))
	}
	// Short enough that the bar never wraps to a second row: a wrapped help
	// bar makes View() one line taller than the terminal, scrolling every
	// visible row up by one and desyncing mouse coordinates from selection.
	h := " 1-4 tabs • ↑/↓ navigate • ←/→ Info/Logs • Space start/stop • r restart • a all • y copy • ? help • Shift+drag select"
	return HelpBarStyle.Width(cw).Render(fitRunes(h, inner))
}

// fitRunes trims s to at most max runes, ending with an ellipsis.
func fitRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
}

func (m Model) renderTabBar() string {
	cw := innerW(m.width)
	items := []string{"[1] Containers", "[2] Images", "[3] Volumes", "[4] Networks"}
	var tabs []string
	for i, item := range items {
		if i == int(m.activeTab) {
			tabs = append(tabs, TabActiveStyle.Render(" "+item+" "))
		} else {
			tabs = append(tabs, TabInactiveStyle.Render(" "+item+" "))
		}
	}
	tabsContent := lipgloss.JoinHorizontal(lipgloss.Top, tabs...)
	tabsContent = lipgloss.Place(cw, 1, lipgloss.Left, lipgloss.Top, tabsContent,
		lipgloss.WithWhitespaceBackground(t.Background),
	)
	line := lipgloss.NewStyle().Background(t.Background).Foreground(t.Border).Render(strings.Repeat("─", cw))
	return lipgloss.JoinVertical(lipgloss.Top, line, tabsContent, line)
}

func (m Model) renderMain() string {
	w := innerW(m.width)
	h := m.height - tabBarHeight - helpBarHeight
	vw := w - 1

	if m.activeTab == tabContainers {
		return m.renderContainersSplit(w, vw, h)
	}

	var hdr, rows string
	if m.err != nil {
		rows = MainPanelStyle.Width(w).Height(h).Render(errorStyle.Render(m.err.Error()))
	} else {
		switch m.activeTab {
		case tabImages:
			hdr, rows = m.renderImageList(w, vw, h)
		case tabVolumes:
			hdr, rows = m.renderVolumeList(w, vw, h)
		case tabNetworks:
			hdr, rows = m.renderNetworkList(w, vw, h)
		}
	}
	m.mainViewport.Width = vw
	m.mainViewport.Height = h - 2
	m.mainViewport.SetContent(rows)
	m.mainViewport.SetYOffset(m.mainYOff)
	m.mainViewport.Style = BaseStyle
	m.mainRows = strings.Count(rows, "\n") + 1
	viewportView := m.mainViewport.View()
	scrollbar := m.renderScrollbar()

	if hdr == "" {
		return MainPanelStyle.Width(w).Height(h).Render(rows)
	}
	return lipgloss.JoinVertical(lipgloss.Top,
		hdr,
		lipgloss.JoinHorizontal(lipgloss.Top, viewportView, scrollbar),
	)
}

func (m Model) renderContainersSplit(w, vw, h int) string {
	topH := int(float64(h) * splitRatio)
	bottomH := h - topH - subTabBarHeight

	var hdr, rows string
	if m.err != nil {
		rows = MainPanelStyle.Width(w).Height(h).Render(errorStyle.Render(m.err.Error()))
	} else {
		hdr, rows = m.renderContainerList(w, vw, h)
	}

	m.mainViewport.Width = vw
	m.mainViewport.Height = topH - 2
	m.mainViewport.SetContent(rows)
	m.mainViewport.SetYOffset(m.mainYOff)
	m.mainViewport.Style = BaseStyle
	m.mainRows = strings.Count(rows, "\n") + 1
	topContent := lipgloss.JoinVertical(lipgloss.Top, hdr,
		lipgloss.JoinHorizontal(lipgloss.Top, m.mainViewport.View(), m.renderScrollbar()),
	)

	subBar := m.renderSubTabBar(w)
	var bottomContent string
	if m.activeSubTab == subTabLogs {
		bottomContent = m.renderSubLogView(w, bottomH)
	} else {
		bottomContent = m.renderContainerDetail(w, bottomH)
	}

	return lipgloss.JoinVertical(lipgloss.Top, topContent, subBar, bottomContent)
}

func (m Model) renderSubTabBar(w int) string {
	items := []string{"Info", "Logs"}
	var tabs []string
	for i, item := range items {
		if i == int(m.activeSubTab) {
			tabs = append(tabs, SubTabActiveStyle.Render(" "+item+" "))
		} else {
			tabs = append(tabs, SubTabInactiveStyle.Render(" "+item+" "))
		}
	}
	bar := lipgloss.JoinHorizontal(lipgloss.Top, tabs...)
	bar = lipgloss.Place(w, 1, lipgloss.Left, lipgloss.Top, bar,
		lipgloss.WithWhitespaceBackground(t.Background),
	)
	line := lipgloss.NewStyle().Background(t.Background).Foreground(t.Border).Render(strings.Repeat("─", w))
	return lipgloss.JoinVertical(lipgloss.Top, line, bar, line)
}

func (m Model) renderContainerDetail(w, bottomH int) string {
	if len(m.containers) == 0 || m.selectedIdx >= len(m.containers) {
		return lipgloss.Place(w, bottomH, lipgloss.Top, lipgloss.Left,
			BaseStyle.Render("  No container selected"),
			lipgloss.WithWhitespaceBackground(t.Background),
		)
	}

	vh := bottomH - 1 // a constant blank gap row at the bottom of the pane
	if vh < 1 {
		vh = 1
	}
	m.detailViewport.Width = w - 1
	m.detailViewport.Height = vh

	content := m.detailStyled
	if m.detailDragSel {
		// Live highlight only while the drag is in flight: the selection is
		// cleared on release, so the stored body never gets re-decorated.
		sel := m.detailSel
		sel.active = true
		content = decorateSelection(content, m.detailContent, sel)
	}
	m.detailViewport.SetContent(content)

	return lipgloss.Place(w, bottomH, lipgloss.Top, lipgloss.Left,
		lipgloss.JoinHorizontal(lipgloss.Top,
			m.detailViewport.View(),
			renderSubScrollbar(m.detailViewport),
		),
		lipgloss.WithWhitespaceBackground(t.Background),
	)
}

// buildDetailContent renders the full Info pane body: fields from the
// containers list plus Networks/Mounts/Labels sections from inspect data.
func (m Model) buildDetailContent(w int) string {
	if len(m.containers) == 0 || m.selectedIdx >= len(m.containers) {
		return ""
	}
	c := m.containers[m.selectedIdx]

	valW := w - 15
	if valW < 8 {
		valW = 8
	}
	// tv sanitizes a value for one labeled row. Newlines inside a Docker
	// value (the ubuntu image's org.opencontainers.image.description is a
	// paragraph) would split the row into several lines; each embedded line
	// shorter than the pane would then be completed by the viewport with
	// unstyled whitespace, showing the default terminal background.
	tv := func(s string) string {
		s = strings.ReplaceAll(s, "\r\n", " ")
		s = strings.ReplaceAll(s, "\n", " ")
		s = strings.ReplaceAll(s, "\r", " ")
		return Truncate(s, valW)
	}

	var b strings.Builder
	// The " Info:" title is part of the scrollable content so it scrolls
	// away with the body.
	titleStyle := lipgloss.NewStyle().Foreground(t.Accent).Bold(true)
	b.WriteString(titleStyle.Render(" Info:") +
		lipgloss.NewStyle().Background(t.Background).
			Render(strings.Repeat(" ", max(w-len([]rune(" Info:")), 0))) + "\n")

	rowStyle := lipgloss.NewStyle().Background(t.Background)
	// padLine pads any row to the full block width w with theme-background
	// spaces - lipgloss pads shorter lines with UNSTYLED whitespace after the
	// last ANSI reset, which would show as default terminal background.
	// A row longer than w is trimmed (with an ellipsis) instead: leaving it
	// would let the viewport word-wrap it into continuation rows that it
	// completes with unstyled whitespace - the same default-background leak -
	// and would desync the one-row-per-buffer-row selection geometry.
	padLine := func(s string) string {
		if rest := w - lipgloss.Width(s); rest > 0 {
			return s + rowStyle.Render(strings.Repeat(" ", rest))
		}
		if lipgloss.Width(s) > w {
			return cutPlain(s, w-1) + rowStyle.Render("…")
		}
		return s
	}
	line := func(label, value string) {
		b.WriteString(padLine(fmt.Sprintf("  %-10s %s", label+":", tv(value))) + "\n")
	}
	// coloredLine renders the value in color when it fits, like colorize().
	coloredLine := func(label, plain string, color lipgloss.Color) {
		b.WriteString(padLine(fmt.Sprintf("  %-10s %s", label+":", colorize(plain, valW, color))) + "\n")
	}

	line("Name", strings.TrimPrefix(c.Names[0], "/"))
	line("ID", shortID(c.ID))
	line("Image", c.Image)
	coloredLine("Status", c.Status, stateColor(c.State))
	coloredLine("State", c.State, stateColor(c.State))
	if cell, ok := renderPortsCell(c.Ports, valW, t.Background); ok {
		b.WriteString(padLine(fmt.Sprintf("  %-10s %s", "Ports:", cell)) + "\n")
	} else {
		line("Ports", formatPorts(c.Ports))
	}
	if c.Created > 0 {
		line("Created", time.Unix(c.Created, 0).Format("2006-01-02 15:04"))
	}
	if c.Command != "" {
		line("Command", c.Command)
	}

	d := m.details
	if d == nil || m.detailsID != c.ID {
		b.WriteString("\n" + padLine("  Loading details…") + "\n")
		return b.String()
	}

	line("Exit code", strconv.Itoa(d.State.ExitCode))
	if d.State.Health != nil && d.State.Health.Status != "" {
		line("Health", d.State.Health.Status)
	}

	section := lipgloss.NewStyle().Foreground(t.Accent).Bold(true)
	writeSection := func(title string) {
		s := " " + title + ":"
		pad := w - len([]rune(s))
		if pad < 0 {
			pad = 0
		}
		b.WriteString("\n" + section.Render(s) + rowStyle.Render(strings.Repeat(" ", pad)) + "\n")
	}

	if len(d.NetworkSettings.Networks) > 0 {
		writeSection("Networks")
		names := make([]string, 0, len(d.NetworkSettings.Networks))
		for n := range d.NetworkSettings.Networks {
			names = append(names, n)
		}
		sort.Strings(names)
		// Column width = longest name in this section (truncation floor 13),
		// capped so the IP never gets pushed off the panel.
		nameCol := 13
		for _, n := range names {
			if l := len([]rune(n)); l > nameCol {
				nameCol = l
			}
		}
		if lim := w - 40; nameCol > lim {
			nameCol = lim
		}
		for _, n := range names {
			b.WriteString(padLine(fmt.Sprintf("  %-*s %s", nameCol, Truncate(n, nameCol), tv(d.NetworkSettings.Networks[n].IPAddress))) + "\n")
		}
	}

	if len(d.Mounts) > 0 {
		writeSection("Mounts")
		for _, mt := range d.Mounts {
			src := mt.Source
			if src == "" && mt.Name != "" {
				src = mt.Name + " (volume)"
			}
			mode := "rw"
			if !mt.RW {
				mode = "ro"
			}
			b.WriteString(padLine(fmt.Sprintf("  %s -> %s (%s)", tv(mt.Destination), tv(src), mode)) + "\n")
		}
	}

	if len(d.Config.Labels) > 0 {
		writeSection("Labels")
		keys := make([]string, 0, len(d.Config.Labels))
		for k := range d.Config.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString(padLine(fmt.Sprintf("  %s=%s", tv(k), tv(d.Config.Labels[k]))) + "\n")
		}
	}

	return strings.TrimRight(b.String(), "\n")
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// styleLogRows repaints the log buffer for the viewport: the rows currently
// visible (yoff..yoff+height in buffer coordinates) are styled with the app's
// theme background/foreground and padded to exactly vw cells, so no cell lets
// the terminal's default colors leak through. Rows covered by sel get a
// highlighted span. Only the visible window is styled (each lipgloss render is
// measurable work), so a drag over a large buffer repaints a handful of rows
// instead of the whole log. The non-visible rows are passed through raw and,
// together with any pad rows, the result keeps the same row count as the
// buffer so the persistent viewport's geometry and scrollbar stay untouched.
func styleLogRows(content string, sel textSel, vw, height, yoff int) string {
	row := lipgloss.NewStyle().Background(t.Background).Foreground(t.Foreground)
	if vw <= 0 {
		vw = 1
	}
	lines := strings.Split(content, "\n")
	top, bot := min(sel.anR, sel.endR), max(sel.anR, sel.endR)
	// Pad a short buffer up to the pane height so the viewport still fills the
	// pane with theme-background rows instead of terminal default cells.
	for len(lines) < max(height, yoff+height) {
		lines = append(lines, "")
	}
	lo := max(0, min(yoff, len(lines)-1))
	hi := min(lo+height, len(lines))
	for i := lo; i < hi; i++ {
		ln := lines[i]
		if !sel.active || i < top || i > bot || len(ln) == 0 {
			// Pad rows and truly empty lines still need the theme background;
			// an empty line inside the selection gets the highlight style.
			if sel.active && i >= top && i <= bot {
				lines[i] = selTextStyle.Width(vw).Render("")
			} else {
				lines[i] = row.Width(vw).Render(ln)
			}
			continue
		}
		from, to, ok := sel.rowSpan(i)
		if !ok {
			lines[i] = row.Width(vw).Render(ln)
			continue
		}
		rs := []rune(ln)
		from = max(0, min(from, len(rs)-1))
		if to < 0 || to >= len(rs) {
			to = len(rs) - 1
		}
		if from > to {
			from, to = to, from
		}
		prefix := string(rs[:from])
		span := string(rs[from : to+1])
		suffix := string(rs[to+1:])
		used := runewidth.StringWidth(prefix) + runewidth.StringWidth(span)
		lines[i] = row.Render(prefix) + selTextStyle.Render(span) +
			row.Copy().Width(max(0, vw-used)).Render(suffix)
	}
	return strings.Join(lines, "\n")
}

// decorateSelection re-renders content with an app-owned text selection
// highlighted. Rows are addressed in buffer coordinates against the plain
// body (which has the same rune layout as the styled one before the trailing
// fill), and the rendered result keeps exactly the same line count as the
// input so the persistent viewport's geometry is untouched. Only the exact
// span [from,to] is painted with the selection style; every other cell keeps
// its original styling - the same partial highlight the Logs pane does.
func decorateSelection(content, plain string, sel textSel) string {
	if !sel.active {
		return content
	}
	lines := strings.Split(content, "\n")
	limit := lines
	if plain != "" {
		limit = strings.Split(plain, "\n")
	} else {
		limit = make([]string, len(lines))
		for i, l := range lines {
			limit[i] = strings.TrimRight(ansiStripped(l), " ")
		}
	}
	top, bot := min(sel.anR, sel.endR), max(sel.anR, sel.endR)
	if top < 0 || top >= len(lines) {
		return content
	}
	for i := top; i <= bot && i < len(lines); i++ {
		from, to, ok := sel.rowSpan(i)
		if !ok {
			continue
		}
		styledN := len([]rune(ansiStripped(lines[i])))
		if styledN == 0 {
			continue
		}
		n := styledN
		if i < len(limit) {
			if ln := len([]rune(limit[i])); ln != 0 {
				n = ln
			}
		}
		from = max(0, min(from, n-1))
		if to < 0 || to >= n {
			to = n - 1
		}
		if from > to {
			from, to = to, from
		}
		lines[i] = highlightStyledSpan(lines[i], from, to)
	}
	return strings.Join(lines, "\n")
}

// highlightStyledSpan re-styles a rendered Info row so exactly the rune span
// [from,to] (visible-rune space of the plain body) is painted with the
// selection background while every other cell keeps its original styling.
// Escape sequences that styled a span rune are dropped (the span is
// re-painted), the ones left of the span stay verbatim, and bare text right
// of the span is re-painted with the theme background so the selection reset
// can never leak default terminal cells.
func highlightStyledSpan(line string, from, to int) string {
	var before, span, after []rune
	in := []rune(line)
	part := 0 // 0 before / 1 span / 2 after
	vis := 0
	i := 0
	for i < len(in) {
		if in[i] == '\x1b' {
			j := i + 1
			for j < len(in) {
				c := in[j]
				if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
					break
				}
				j++
			}
			if j >= len(in) {
				break
			}
			// Sequences styling a span rune are swallowed: [from,to] is
			// re-painted wholesale, so carrying them over would double-apply
			// backgrounds and drop the span's own color accents.
			if (part == 0 && vis < from) || part == 2 {
				out := &before
				if part == 2 {
					out = &after
				}
				*out = append(*out, in[i:j+1]...)
			}
			i = j + 1
			continue
		}
		if part == 0 && vis == from {
			part = 1
		}
		switch part {
		case 0:
			before = append(before, in[i])
		case 1:
			span = append(span, in[i])
		case 2:
			after = append(after, in[i])
		}
		vis++
		i++
		if part == 1 && vis > to {
			part = 2
		}
	}

	base := lipgloss.NewStyle().Background(t.Background).Foreground(t.Foreground)
	return string(before) + selTextStyle.Render(string(span)) + repaintBare(after, base)
}

// repaintBare re-emits the tail of a split styled row: escape sequences are
// kept verbatim (they carry their own background/color), bare text runs are
// re-painted with base. Without this, bare text right of the selection reset
// would fall back to the default terminal background.
func repaintBare(after []rune, base lipgloss.Style) string {
	var b strings.Builder
	i := 0
	for i < len(after) {
		if after[i] == '\x1b' {
			j := i + 1
			for j < len(after) {
				c := after[j]
				if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
					break
				}
				j++
			}
			if j >= len(after) {
				return b.String() + string(after[i:])
			}
			b.WriteString(string(after[i : j+1]))
			i = j + 1
			continue
		}
		j := i
		for j < len(after) && after[j] != '\x1b' {
			j++
		}
		b.WriteString(base.Render(string(after[i:j])))
		i = j
	}
	return b.String()
}

// selectedText returns the visible text covered by the selection, one line
// per buffer row, in the order it appears on screen.
func selectedText(content string, sel textSel) string {
	if !sel.active {
		return ""
	}
	lines := strings.Split(content, "\n")
	top, bot := min(sel.anR, sel.endR), max(sel.anR, sel.endR)
	if top < 0 || top >= len(lines) {
		return ""
	}
	var sb strings.Builder
	for i := top; i <= bot && i < len(lines); i++ {
		line := lines[i]
		if strings.ContainsRune(line, '\x1b') {
			line = ansiStripped(line)
		}
		from, to, ok := sel.rowSpan(i)
		if !ok {
			continue
		}
		n := len([]rune(line))
		if to < 0 || to >= n {
			to = n - 1
		}
		if n == 0 {
			continue
		}
		from = max(0, min(from, n-1))
		if from > to {
			from, to = to, from
		}
		sb.WriteString(string([]rune(line)[from : to+1]))
		if i < bot {
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

func (m Model) renderSubLogView(w, bottomH int) string {
	if len(m.containers) == 0 || m.selectedIdx >= len(m.containers) {
		return lipgloss.Place(w, bottomH, lipgloss.Top, lipgloss.Left,
			BaseStyle.Render("  No container selected"),
			lipgloss.WithWhitespaceBackground(t.Background),
		)
	}
	c := m.containers[m.selectedIdx]
	name := strings.TrimPrefix(c.Names[0], "/")

	rowStyle := lipgloss.NewStyle().Background(t.Background)
	followGlyphStyle := rowStyle.Copy()
	followLabelStyle := rowStyle.Copy()
	if m.logFollow {
		followGlyphStyle = followGlyphStyle.Foreground(t.Accent)
		followLabelStyle = followLabelStyle.Foreground(t.Foreground)
	} else {
		followGlyphStyle = followGlyphStyle.Foreground(t.Muted)
		followLabelStyle = followLabelStyle.Foreground(t.Muted)
	}
	headerLeft := " Logs: " + name + "  "
	header := lipgloss.JoinHorizontal(lipgloss.Top,
		rowStyle.Copy().Foreground(t.Accent).Bold(true).Render(" Logs: "),
		rowStyle.Copy().Foreground(t.Foreground).Render(name),
		rowStyle.Copy().Foreground(t.Muted).Render("  "),
		followGlyphStyle.Render(m.followCheckboxGlyph()),
		rowStyle.Copy().Foreground(t.Muted).Render(" "),
		followLabelStyle.Render("follow"),
		rowStyle.Render(strings.Repeat(" ", w-len([]rune(headerLeft+m.followCheckboxText())))),
	)

	sel := m.logSel
	if m.dragSel {
		// Live highlight only while the drag is in flight: the selection is
		// cleared on release, so the band renders unstyled the rest of the time.
		sel.active = true
	}
	m.containerLogViewport.Width = w - 1              // viewport shares the pane with the scrollbar column
	m.containerLogViewport.Height = max(bottomH-3, 1) // header + separator + blank gap row
	// The buffer is already wrapped at fetch time; styleLogRows repaints every
	// row (and the trailing cells) with the app's theme background so the
	// terminal default never shows through, keeping the line count identical.
	styled := styleLogRows(m.containerLogContent, sel, w-1, m.containerLogViewport.Height, m.containerLogViewport.YOffset)
	m.containerLogViewport.SetContent(styled)

	sep := lipgloss.NewStyle().Background(t.Background).Foreground(t.Border).Render(strings.Repeat("─", w))
	content := lipgloss.JoinVertical(lipgloss.Top, header, sep,
		lipgloss.JoinHorizontal(lipgloss.Top,
			m.containerLogViewport.View(),
			renderSubScrollbar(m.containerLogViewport),
		),
	)
	return lipgloss.Place(w, bottomH, lipgloss.Top, lipgloss.Left, content,
		lipgloss.WithWhitespaceBackground(t.Background),
	)
}

// followCheckboxGlyph renders the follow-the-tail status circle: a filled
// ● when following the tail, a hollow ○ when not, mirroring the containers
// table's state dots (color is applied by the header styles).
func (m Model) followCheckboxGlyph() string {
	if m.logFollow {
		return "●"
	}
	return "○"
}

// followCheckboxText is the combined glyph+label, used as the size oracle
// for click-zones and header padding, matching the rendered row.
func (m Model) followCheckboxText() string {
	return m.followCheckboxGlyph() + " follow"
}

// followCheckboxCols returns the column range (content x) of the follow
// checkbox in the Logs pane header, matching followCheckboxText.
func (m Model) followCheckboxCols() (start, end int) {
	if len(m.containers) == 0 || m.selectedIdx >= len(m.containers) {
		return -1, -1
	}
	name := strings.TrimPrefix(m.containers[m.selectedIdx].Names[0], "/")
	start = lipgloss.Width(" Logs: " + name + "  ")
	return start, start + lipgloss.Width(m.followCheckboxText())
}

// renderVScroll renders a 1-column vertical scrollbar for a viewport of
// the given height showing totalRows lines at scroll progress pct.
func renderVScroll(vh, totalRows int, pct float64) string {
	if vh <= 0 || totalRows <= vh {
		return BaseStyle.Width(1).Height(max(vh, 0)).Render(" ")
	}
	thumbH := 3
	if vh < thumbH {
		thumbH = vh
	}
	maxThumb := vh - thumbH
	thumbPos := int(pct * float64(maxThumb))
	var sb strings.Builder
	for i := 0; i < vh; i++ {
		if i >= thumbPos && i < thumbPos+thumbH {
			sb.WriteString(lipgloss.NewStyle().Background(t.Border).Foreground(t.Muted).Width(1).Render(" "))
		} else {
			sb.WriteString(lipgloss.NewStyle().Background(t.Background).Foreground(t.Border).Width(1).Render(" "))
		}
		if i < vh-1 {
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// renderScrollbar is the main list scrollbar.
func (m Model) renderScrollbar() string {
	return renderVScroll(m.mainViewport.Height, m.mainRows, m.mainViewport.ScrollPercent())
}

// renderSubScrollbar is the scrollbar for a bottom-pane (sub-menu) viewport.
func renderSubScrollbar(v viewport.Model) string {
	return renderVScroll(v.Height, v.TotalLineCount(), v.ScrollPercent())
}

// fitMainViewport syncs the real mainViewport's size and content with what
// the render path will display. Render helpers run on value copies of Model,
// so without this the persistent viewport keeps stale dimensions and
// bubbles-native scrolling (mouse wheel) silently no-ops.
func (m *Model) fitMainViewport() {
	w := innerW(m.width)
	vw := w - 1
	h := m.height - tabBarHeight - helpBarHeight

	var rows string
	if m.activeTab == tabContainers {
		topH := int(float64(h) * splitRatio)
		_, rows = m.renderContainerList(w, vw, h)
		m.mainViewport.Width = vw
		m.mainViewport.Height = topH - 2
	} else {
		switch m.activeTab {
		case tabImages:
			_, rows = m.renderImageList(w, vw, h)
		case tabVolumes:
			_, rows = m.renderVolumeList(w, vw, h)
		case tabNetworks:
			_, rows = m.renderNetworkList(w, vw, h)
		}
		m.mainViewport.Width = vw
		m.mainViewport.Height = h - 2
	}
	if rows == "" {
		rows = MainPanelStyle.Width(w).Height(h).Render("")
	}
	m.mainViewport.SetContent(rows)
	m.mainViewport.SetYOffset(m.mainYOff)
}

// fitViewports syncs every viewport whose persistent state matters for
// bubbles-native scrolling.
func (m *Model) fitViewports() {
	m.fitMainViewport()
	m.fitDetailViewport()
	m.fitContainerLogViewport()
}

// fitContainerLogViewport re-wraps the stored logs for the current width
// and keeps the persistent log viewport's dimensions in sync with the
// bottom pane layout - stale dimensions would anchor AtBottom/GotoBottom
// to the wrong offsets and hide the freshest lines below the visible area.
func (m *Model) fitContainerLogViewport() {
	h := m.height - tabBarHeight - helpBarHeight
	topH := int(float64(h) * splitRatio)
	bottomH := h - topH - subTabBarHeight
	vh := max(bottomH-3, 1) // header + separator + blank gap row, same as renderSubLogView

	m.containerLogViewport.Width = innerW(m.width) - 1
	m.containerLogViewport.Height = vh
	if m.containerLogContent == "" {
		return
	}
	m.containerLogViewport.SetContent(m.containerLogContent)
}

// fitDetailViewport syncs the detail (Info) viewport's size and content on
// the persistent model, mirroring the bottom pane of the containers split.
func (m *Model) fitDetailViewport() {
	w := innerW(m.width)
	h := m.height - tabBarHeight - helpBarHeight
	topH := int(float64(h) * splitRatio)
	bottomH := h - topH - subTabBarHeight

	vh := bottomH - 1 // a constant blank gap row at the bottom of the pane
	if vh < 1 {
		vh = 1
	}
	cw := w - 1 // viewport shares the pane with the scrollbar column
	m.detailViewport.Width = cw
	m.detailViewport.Height = vh
	content := m.buildDetailContent(cw)
	if content == "" {
		content = "  No container selected"
	}
	// Keep the plain body around as the selection buffer. When the Info
	// content genuinely changes (details arrive, container switch) an active
	// selection re-anchors on the matching text, like the Logs full reloads.
	m.detailStyled = content
	// padLine fills every row to the pane width with theme-styled spaces so a
	// selection reset can never leak default terminal cells. Those fill spaces
	// must not become part of a multi-row drag (they were highlighted AND
	// copied), so the plain buffer stores the rows trimmed to their text.
	plain := ansiStripped(content)
	{
		rows := strings.Split(plain, "\n")
		for i := range rows {
			rows[i] = strings.TrimRight(rows[i], " ")
		}
		plain = strings.Join(rows, "\n")
	}
	if prev := m.detailContent; prev != "" && prev != plain && m.detailSel.active {
		if s, ok := reanchorSelection(prev, m.detailSel, plain); ok {
			m.detailSel = s
		} else {
			m.detailSel = textSel{}
			m.detailDragSel = false
		}
	}
	m.detailContent = plain
	m.detailViewport.SetContent(content)
}

// maybeReloadContainerLogs reloads the logs of the newly selected container
// when the selection changed while the Logs sub-tab is active.
func (m Model) maybeReloadContainerLogs(oldIdx int) tea.Cmd {
	if oldIdx != m.selectedIdx && m.activeTab == tabContainers && m.activeSubTab == subTabLogs {
		return m.loadContainerLogs()
	}
	return nil
}

// maybeLoadContainerDetails fetches inspect data for the newly selected
// container when the selection changed while the Info sub-tab is active.
func (m Model) maybeLoadContainerDetails(oldIdx int) tea.Cmd {
	if oldIdx != m.selectedIdx && m.activeTab == tabContainers && m.activeSubTab == subTabInfo {
		return m.loadContainerDetails()
	}
	return nil
}

// selectionChangedCmds returns commands to run after the selected row moves.
func (m Model) selectionChangedCmds(oldIdx int) tea.Cmd {
	return tea.Batch(
		m.maybeReloadContainerLogs(oldIdx),
		m.maybeLoadContainerDetails(oldIdx),
	)
}

// loadContainerDetails asynchronously inspects the selected container.
// Skips the request when details for this exact container are already loaded.
func (m Model) loadContainerDetails() tea.Cmd {
	if m.activeTab != tabContainers || m.selectedIdx >= len(m.containers) {
		return nil
	}
	c := m.containers[m.selectedIdx]
	if m.detailsID == c.ID {
		return nil
	}
	return func() tea.Msg {
		d, err := m.docker.InspectContainer(c.ID)
		if err != nil {
			return silentErrMsg{err}
		}
		return containerDetailsMsg{id: c.ID, details: d}
	}
}

// mouseInBottomPane reports whether the mouse cursor is over the bottom
// half of the containers split view, including the sub-tab bar strip -
// wheel events there scroll the active sub-pane.
func (m Model) mouseInBottomPane(y int) bool {
	contentH := m.height - tabBarHeight - helpBarHeight
	topH := int(float64(contentH) * splitRatio)
	absY := y - tabBarHeight
	return absY >= topH
}

// logContentY returns the viewport-relative row for a screen y inside the
// Logs pane, or -1 when the y is not over the content band.
func (m Model) logContentY(y int) int {
	contentH := m.height - tabBarHeight - helpBarHeight
	topH := int(float64(contentH) * splitRatio)
	return y - tabBarHeight - (topH + 5)
}

// logScreenToCell maps a screen (x, y) onto a buffer cell (row, rune
// column). With clamp=true, off-body coordinates are snapped to the nearest
// visible content cell so a drag released just outside the pane still
// resolves as an edge selection; a value copy with clamp=false rejects
// anything outside the log body.
func (m Model) logScreenToCell(x, y int, clamp bool) (row, col int, ok bool) {
	if m.width == 0 {
		return 0, 0, false
	}
	if x < appMarginX || x >= m.width-appMarginX {
		if !clamp {
			return 0, 0, false
		}
		x = max(appMarginX, min(x, m.width-appMarginX-1))
	}
	x -= appMarginX
	vw := innerW(m.width) - 1
	if x < 0 || x >= vw {
		if !clamp {
			return 0, 0, false
		}
		x = max(0, min(x, vw-1))
	}
	vrow := m.logContentY(y)
	if vrow < 0 || vrow >= m.containerLogViewport.Height {
		if !clamp {
			return 0, 0, false
		}
		vrow = max(0, min(vrow, m.containerLogViewport.Height-1))
	}
	lines := strings.Split(m.containerLogContent, "\n")
	row = vrow + m.containerLogViewport.YOffset
	if row < 0 || row >= len(lines) {
		return 0, 0, false
	}
	if col, ok := cellToRuneColumn(lines[row], x); ok {
		return row, col, true
	}
	return 0, 0, false
}

// cellToRuneColumn maps a terminal cell x onto the rune index that occupies it
// in line. Lines are pre-wrapped in cell space and stored plain, so a rune's
// cell and its rune index diverge only for double-cell runes: x landing on
// either half of such a rune resolves to that rune, and x beyond the end maps
// to the last rune. The col returned is an index into []rune(line).
func cellToRuneColumn(line string, cellX int) (col int, ok bool) {
	col = 0
	cells := 0
	for i, r := range []rune(line) {
		w := runewidth.RuneWidth(r)
		if cells+w > cellX {
			return i, true
		}
		cells += w
		col = i
	}
	if cellX > cells {
		col = len([]rune(line)) - 1
	}
	if col < 0 {
		col = 0
	}
	return col, true
}

// detailContentY returns the viewport-relative row for a screen y inside the
// Info pane, or -1 when the y is not over the content band. Unlike the Logs
// pane, the Info body starts directly under the 3-row sub-tab strip (no
// header/separator rows).
func (m Model) detailContentY(y int) int {
	contentH := m.height - tabBarHeight - helpBarHeight
	topH := int(float64(contentH) * splitRatio)
	return y - tabBarHeight - (topH + subTabBarHeight)
}

// detailScreenToCell maps a screen (x, y) onto a buffer cell (row, rune
// column) of the plain Info body, mirroring logScreenToCell's snapping.
func (m Model) detailScreenToCell(x, y int, clamp bool) (row, col int, ok bool) {
	if m.width == 0 {
		return 0, 0, false
	}
	if x < appMarginX || x >= m.width-appMarginX {
		if !clamp {
			return 0, 0, false
		}
		x = max(appMarginX, min(x, m.width-appMarginX-1))
	}
	x -= appMarginX
	vw := innerW(m.width) - 1
	if x < 0 || x >= vw {
		if !clamp {
			return 0, 0, false
		}
		x = max(0, min(x, vw-1))
	}
	vrow := m.detailContentY(y)
	if vrow < 0 || vrow >= m.detailViewport.Height {
		if !clamp {
			return 0, 0, false
		}
		vrow = max(0, min(vrow, m.detailViewport.Height-1))
	}
	lines := strings.Split(m.detailContent, "\n")
	row = vrow + m.detailViewport.YOffset
	if row < 0 || row >= len(lines) {
		return 0, 0, false
	}
	n := len([]rune(lines[row]))
	col = x
	if col >= n {
		col = n - 1
	}
	if col < 0 {
		col = 0
	}
	return row, col, true
}

// finishDetailSelection finalizes an in-flight Info drag, mirroring
// finishLogSelection: click leaves it inactive, a real drag copies the plain
// span to the clipboard via OSC 52 and immediately clears the highlight.
func (m *Model) finishDetailSelection(x, y int) tea.Cmd {
	m.detailDragSel = false
	if r, c, ok := m.detailScreenToCell(x, y, true); ok {
		m.detailSel.endR, m.detailSel.endC = r, c
	}
	cmd := osc52CopyText(m.detailContent, m.detailSel)
	m.detailSel = textSel{}
	return cmd
}

// finishLogSelection finalizes an in-flight drag: the endpoint snaps to the
// nearest content cell and a click (no movement) leaves the selection cleared
// so a plain click clears a previous highlight. A non-trivial drag copies the
// selected text to the host clipboard via OSC 52, then immediately drops the
// highlight.
func (m *Model) finishLogSelection(x, y int) tea.Cmd {
	m.dragSel = false
	if r, c, ok := m.logScreenToCell(x, y, true); ok {
		m.logSel.endR, m.logSel.endC = r, c
	}
	cmd := osc52CopyText(m.containerLogContent, m.logSel)
	m.logSel = textSel{}
	return cmd
}

// osc52CopyText copies the given plain span to the host clipboard via OSC 52
// when it covers a non-trivial span. It distinguishes a real drag (anchor !=
// endpoint, e.g. an in-flight drag whose live highlight hasn't flipped active
// yet) from a plain click (anchor == endpoint), so a click never emits a
// sequence. The caller is responsible for clearing the selection afterwards.
func osc52CopyText(content string, sel textSel) tea.Cmd {
	if sel.isTrivial() {
		return nil
	}
	sel.active = true // selectedText only yields text for an active span
	return osc52Copy(selectedText(content, sel))
}

// selectionText returns the plain (ANSI-stripped) text currently selected in
// the Logs pane.
func (m Model) selectionText() string {
	return selectedText(m.containerLogContent, m.logSel)
}

// osc52Copy returns a Cmd that writes payload to the terminal clipboard using
// the OSC 52 escape sequence. The app runs in the alternate screen where
// tea.Println/tea.Printf suppress output, so the sequence is written straight
// to the terminal; it only updates the host clipboard and paints nothing.
func osc52Copy(payload string) tea.Cmd {
	return func() tea.Msg {
		fmt.Fprint(os.Stdout, osc52Sequence(payload))
		return nil
	}
}

// osc52Sequence builds the raw OSC 52 clipboard-write escape sequence for
// payload: 52 is the clipboard pseudo-function, c the selection clipboard.
func osc52Sequence(payload string) string {
	b64 := base64.StdEncoding.EncodeToString([]byte(payload))
	return "\x1b]52;c;" + b64 + "\x1b\\"
}

// reanchorSelection maps a selection anchored in oldContent onto newContent
// by relocating the selected text. It returns the new selection and whether
// the text is still present. Identical content (a redundant full reload)
// keeps the selection untouched.
func reanchorSelection(oldContent string, oldSel textSel, newContent string) (textSel, bool) {
	if !oldSel.active {
		return textSel{}, false
	}
	if newContent == oldContent {
		return oldSel, true
	}
	text := selectedText(oldContent, oldSel)
	if text == "" {
		return textSel{}, false
	}
	start := strings.Index(newContent, text)
	if start < 0 {
		return textSel{}, false
	}
	end := start + len(text)
	anR, anC := cellAt(newContent, len([]rune(newContent[:start])))
	// logSel end columns are inclusive, so back up one rune off the end.
	endR, endC := cellAt(newContent, len([]rune(newContent[:end]))-1)
	return textSel{active: true, anR: anR, anC: anC, endR: endR, endC: endC}, true
}

// cellAt returns the (row, rune-column) inside content for a rune offset.
// content is the pre-wrap, plain-text log buffer (no ANSI).
func cellAt(content string, r int) (int, int) {
	for i, line := range strings.Split(content, "\n") {
		n := len([]rune(line))
		if r <= n {
			return i, r
		}
		r -= n + 1 // +1 for the separating newline
	}
	return 0, 0
}

func (m Model) handleClick(x, y int) (Model, tea.Cmd) {
	// Ignore clicks in the global horizontal margins and translate the
	// x coordinate into the content area.
	if x < appMarginX || x >= m.width-appMarginX {
		return m, nil
	}
	x -= appMarginX

	// Global tabs: y=0~2 (line + tabs + line)
	if y >= 0 && y <= 2 {
		items := []string{"[1] Containers", "[2] Images", "[3] Volumes", "[4] Networks"}
		var tabBorders []int
		cum := 0
		for _, item := range items {
			w := lipgloss.Width(TabInactiveStyle.Render(" " + item + " "))
			cum += w
			tabBorders = append(tabBorders, cum)
		}
		for i, border := range tabBorders {
			if x < border {
				m.switchTab(tab(i))
				return m, m.refreshNow()
			}
		}
		return m, nil
	}

	contentH := m.height - tabBarHeight - helpBarHeight
	topH := int(float64(contentH) * splitRatio)

	if m.activeTab == tabContainers {
		absY := y - tabBarHeight

		// Sub-tab bar area (3 rows: line + tabs + line). The whole strip
		// is clickable so aiming one row off still hits the buttons.
		if absY >= topH && absY < topH+3 {
			subItems := []string{"Info", "Logs"}
			cum := 0
			for i, item := range subItems {
				w := lipgloss.Width(SubTabInactiveStyle.Render(" " + item + " "))
				cum += w
				if x < cum {
					m.activeSubTab = subTab(i)
					if i == int(subTabInfo) {
						m.logSel = textSel{}
						m.dragSel = false
						return m, m.loadContainerDetails()
					}
					if i == int(subTabLogs) {
						m.detailSel = textSel{}
						m.detailDragSel = false
						return m, m.loadContainerLogs()
					}
				}
			}
			return m, nil
		}

		// Logs pane header: click on the follow checkbox toggles it.
		if m.activeTab == tabContainers && m.activeSubTab == subTabLogs &&
			absY == topH+3 && len(m.containers) > 0 && m.selectedIdx < len(m.containers) {
			if start, end := m.followCheckboxCols(); x >= start && x < end {
				m.logFollow = !m.logFollow
				return m, nil
			}
		}

		// Table rows
		if absY < topH {
			rowY := absY - 2 + m.mainYOff
			if rowY >= 0 && rowY < len(m.containers) {
				m.selectedIdx = rowY
				if m.activeSubTab == subTabLogs {
					return m, m.loadContainerLogs()
				}
				return m, m.loadContainerDetails()
			}
			return m, nil
		}
		return m, nil
	}

	// Other tabs
	absY := y - tabBarHeight
	if absY >= 0 {
		rowY := absY - 2 + m.mainYOff
		maxIdx := len(m.containers) - 1
		switch m.activeTab {
		case tabImages:
			maxIdx = len(m.images) - 1
		case tabVolumes:
			maxIdx = len(m.volumes) - 1
		case tabNetworks:
			maxIdx = len(m.networks) - 1
		}
		if rowY >= 0 && rowY <= maxIdx {
			m.selectedIdx = rowY
		}
	}
	return m, nil
}

func (m Model) renderContainerList(w, vw, h int) (string, string) {
	colW := vw
	if m.loading && len(m.containers) == 0 {
		return "", MainPanelStyle.Width(w).Height(h).Render(BaseStyle.Foreground(t.Muted).Render(" Waiting for Docker…"))
	}
	if len(m.containers) == 0 {
		return "", MainPanelStyle.Width(w).Height(h).Render(BaseStyle.Foreground(t.Muted).Render(" No containers found"))
	}
	hdr := fmt.Sprintf("     %-29s %-11s  %-32s  %-34s", "NAME", "STATE", "IMAGE", "PORTS")
	if pad := w - len([]rune(hdr)); pad > 0 {
		hdr += strings.Repeat(" ", pad)
	}
	header := lipgloss.NewStyle().Background(t.Background).Foreground(t.Accent).Bold(true).Render(hdr)
	sep := lipgloss.NewStyle().Background(t.Background).Foreground(t.Border).Render(strings.Repeat("─", w))

	var rows []string
	for i, c := range m.containers {
		name := Truncate(strings.TrimPrefix(c.Names[0], "/"), 29)
		state := Truncate(c.State, 11)
		ports := formatPorts(c.Ports)
		img := Truncate(c.Image, 32)

		dot := "●"
		if c.State == "exited" {
			dot = "○"
		}
		dotColor := stateColor(c.State)

		line := fmt.Sprintf(" %s  %-29s %-11s  %-32s  %-34s", dot, name, state, img, ports)
		runes := []rune(line)
		padding := colW - len(runes)
		if padding > 0 {
			line = line + strings.Repeat(" ", padding)
			runes = []rune(line)
		} else if padding < 0 {
			runes = runes[:max(colW, 3)]
		}

		seg := func(from, to int) string {
			if from > len(runes) {
				from = len(runes)
			}
			if to > len(runes) {
				to = len(runes)
			}
			if from > to {
				from = to
			}
			return string(runes[from:to])
		}

		bg := t.Background
		if i == m.selectedIdx {
			bg = lipgloss.Color("#2d4a2e")
		}
		bgStyle := lipgloss.NewStyle().Background(bg)

		// Ports: use the protocol-colored cell (which fills the remaining
		// width with its own background); fall back to the plain segment
		// when the list does not fit.
		avail := colW - portsCol
		if avail < 0 {
			avail = 0
		}
		var portsRow string
		if cell, ok := renderPortsCell(c.Ports, avail, bg); ok {
			portsRow = cell
		} else {
			portsRow = bgStyle.Render(seg(portsCol, len(runes)))
		}

		row := bgStyle.Render(" ") +
			bgStyle.Copy().Foreground(dotColor).Render(seg(1, 2)) +
			bgStyle.Foreground(t.Foreground).Render(seg(2, stateCol)) +
			bgStyle.Copy().Foreground(stateColor(c.State)).Render(seg(stateCol, imageCol)) +
			bgStyle.Foreground(t.Foreground).Render(seg(imageCol, portsCol)) +
			portsRow
		rows = append(rows, row)
	}
	return header + "\n" + sep, lipgloss.JoinVertical(lipgloss.Top, rows...)
}

func (m Model) renderImageList(w, vw, h int) (string, string) {
	colW := vw
	if m.loading && len(m.images) == 0 {
		return "", MainPanelStyle.Width(w).Height(h).Render(BaseStyle.Foreground(t.Muted).Render(" Loading images…"))
	}
	if len(m.images) == 0 {
		return "", MainPanelStyle.Width(w).Height(h).Render(BaseStyle.Foreground(t.Muted).Render(" No images found"))
	}

	hdr := fmt.Sprintf("     %-42s %-16s  %-20s  %-14s", "REPOSITORY:TAG", "IMAGE ID", "CREATED", "SIZE")
	padding := w - len([]rune(hdr))
	if padding > 0 {
		hdr += strings.Repeat(" ", padding)
	}
	header := lipgloss.NewStyle().Background(t.Background).Foreground(t.Accent).Bold(true).Render(hdr)
	sep := lipgloss.NewStyle().Background(t.Background).Foreground(t.Border).Render(strings.Repeat("─", w))

	var rows []string
	for i := range m.images {
		img := &m.images[i]
		repoTag := "<none>:<none>"
		if len(img.RepoTags) > 0 && img.RepoTags[0] != "<none>:<none>" {
			repoTag = img.RepoTags[0]
		}
		repoTag = Truncate(repoTag, 42)

		shortID := Truncate(img.ID, 16)
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}

		created := formatCreated(img.Created)
		created = Truncate(created, 20)

		size := formatImageSize(img.Size)
		size = Truncate(size, 14)

		line := fmt.Sprintf(" %s  %-42s %-16s  %-20s  %-14s", "◎", repoTag, shortID, created, size)
		runes := []rune(line)
		pad := colW - len(runes)
		if pad > 0 {
			line += strings.Repeat(" ", pad)
			runes = []rune(line)
		} else if pad < 0 {
			runes = runes[:max(colW, 3)]
			line = string(runes)
		}

		bg := t.Background
		if i == m.selectedIdx {
			bg = lipgloss.Color("#2d4a2e")
		}
		bgStyle := lipgloss.NewStyle().Background(bg)
		row := bgStyle.Render(" ") +
			bgStyle.Foreground(t.Foreground).Render(string(runes[1:]))
		rows = append(rows, row)
	}
	return header + "\n" + sep, lipgloss.JoinVertical(lipgloss.Top, rows...)
}

func formatImageSize(bytes int64) string {
	switch {
	case bytes >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(bytes)/float64(1<<30))
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(bytes)/float64(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(bytes)/float64(1<<10))
	default:
		return fmt.Sprintf("%dB", bytes)
	}
}

func formatCreated(created int64) string {
	t := time.Unix(created, 0)
	return t.Format("2006-01-02")
}

func (m Model) renderVolumeList(w, vw, h int) (string, string) {
	colW := vw
	if m.loading && len(m.volumes) == 0 {
		return "", MainPanelStyle.Width(w).Height(h).Render(BaseStyle.Foreground(t.Muted).Render(" Loading volumes…"))
	}
	if len(m.volumes) == 0 {
		return "", MainPanelStyle.Width(w).Height(h).Render(BaseStyle.Foreground(t.Muted).Render(" No volumes found"))
	}

	hdr := fmt.Sprintf("     %-24s %-16s  %-30s  %-14s", "NAME", "DRIVER", "MOUNTPOINT", "SCOPE")
	padding := w - len([]rune(hdr))
	if padding > 0 {
		hdr += strings.Repeat(" ", padding)
	}
	header := lipgloss.NewStyle().Background(t.Background).Foreground(t.Accent).Bold(true).Render(hdr)
	sep := lipgloss.NewStyle().Background(t.Background).Foreground(t.Border).Render(strings.Repeat("─", w))

	var rows []string
	for i := range m.volumes {
		v := &m.volumes[i]
		name := Truncate(v.Name, 24)
		driver := Truncate(v.Driver, 16)
		mp := Truncate(v.Mountpoint, 30)
		scope := Truncate(v.Scope, 14)

		line := fmt.Sprintf(" %s  %-24s %-16s  %-30s  %-14s", "●", name, driver, mp, scope)
		runes := []rune(line)
		pad := colW - len(runes)
		if pad > 0 {
			line += strings.Repeat(" ", pad)
			runes = []rune(line)
		} else if pad < 0 {
			runes = runes[:max(colW, 3)]
			line = string(runes)
		}

		bg := t.Background
		if i == m.selectedIdx {
			bg = lipgloss.Color("#2d4a2e")
		}
		bgStyle := lipgloss.NewStyle().Background(bg)
		row := bgStyle.Render(" ") +
			bgStyle.Foreground(t.Foreground).Render(string(runes[1:]))
		rows = append(rows, row)
	}
	return header + "\n" + sep, lipgloss.JoinVertical(lipgloss.Top, rows...)
}

func (m Model) renderNetworkList(w, vw, h int) (string, string) {
	colW := vw
	if m.loading && len(m.networks) == 0 {
		return "", MainPanelStyle.Width(w).Height(h).Render(BaseStyle.Foreground(t.Muted).Render(" Loading networks…"))
	}
	if len(m.networks) == 0 {
		return "", MainPanelStyle.Width(w).Height(h).Render(BaseStyle.Foreground(t.Muted).Render(" No networks found"))
	}

	hdr := fmt.Sprintf("     %-28s %-16s  %-16s  %-16s", "NAME", "DRIVER", "ID", "SCOPE")
	padding := w - len([]rune(hdr))
	if padding > 0 {
		hdr += strings.Repeat(" ", padding)
	}
	header := lipgloss.NewStyle().Background(t.Background).Foreground(t.Accent).Bold(true).Render(hdr)
	sep := lipgloss.NewStyle().Background(t.Background).Foreground(t.Border).Render(strings.Repeat("─", w))

	var rows []string
	for i := range m.networks {
		n := &m.networks[i]
		name := Truncate(n.Name, 28)
		driver := Truncate(n.Driver, 16)
		shortID := Truncate(n.ID, 16)
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}
		scope := Truncate(n.Scope, 16)

		line := fmt.Sprintf(" %s  %-28s %-16s  %-16s  %-16s", "●", name, driver, shortID, scope)
		runes := []rune(line)
		pad := colW - len(runes)
		if pad > 0 {
			line += strings.Repeat(" ", pad)
			runes = []rune(line)
		} else if pad < 0 {
			runes = runes[:max(colW, 3)]
			line = string(runes)
		}

		bg := t.Background
		if i == m.selectedIdx {
			bg = lipgloss.Color("#2d4a2e")
		}
		bgStyle := lipgloss.NewStyle().Background(bg)
		row := bgStyle.Render(" ") +
			bgStyle.Foreground(t.Foreground).Render(string(runes[1:]))
		rows = append(rows, row)
	}
	return header + "\n" + sep, lipgloss.JoinVertical(lipgloss.Top, rows...)
}

func formatPorts(ports []docker.Port) string {
	if len(ports) == 0 {
		return ""
	}
	var parts []string
	for _, p := range ports {
		if p.PublicPort != 0 {
			parts = append(parts, fmt.Sprintf("%d->%d/%s", p.PublicPort, p.PrivatePort, p.Type))
		} else {
			parts = append(parts, fmt.Sprintf("%d/%s", p.PrivatePort, p.Type))
		}
	}
	return strings.Join(parts, ", ")
}

// wrapLogCells turns raw docker log content into plain, pre-wrapped buffer
// text whose rows occupy no more than width terminal cells. ANSI SGR escapes
// and CR bytes are stripped first, so the buffer carries only the visible
// text; wrapping then happens in cell space (runewidth) rather than rune
// space, breaking on rune boundaries and never splitting a double-cell rune.
// Terminals hard-wrap text by CELLS, so wrapping by runes would let their own
// re-wrap push real rows apart from buffer rows and break click-to-cell
// mapping; with cell-space wrapping the buffer rows and the rendered rows
// are always identical.
func wrapLogCells(content string, width int) string {
	content = ansiStripped(strings.ReplaceAll(content, "\r", ""))
	if width <= 0 {
		return content
	}
	// Terminals render a tab as a jump to the next tab stop (every 8
	// columns), but runewidth measures it as 0 cells. Expanding tabs to
	// spaces here keeps the runewidth-based cell accounting identical to the
	// terminal's and lipgloss's, so wrapped rows never visually exceed
	// `width` cells and the row/column mapping stays authoritative.
	content = expandTabs(content, 8)
	var result strings.Builder
	for _, line := range strings.Split(content, "\n") {
		var cur []rune
		cells := 0
		for _, r := range line {
			w := runewidth.RuneWidth(r)
			if cells > 0 && cells+w > width {
				result.WriteString(string(cur))
				result.WriteByte('\n')
				cur = cur[:0]
				cells = 0
			}
			cur = append(cur, r)
			cells += w
		}
		result.WriteString(string(cur))
		result.WriteByte('\n')
	}
	return strings.TrimRight(result.String(), "\n")
}

// expandTabs replaces each tab with spaces that advance to the next tab stop
// (columns evenly divisible by tabWidth, matching a default terminal).
func expandTabs(s string, tabWidth int) string {
	if tabWidth <= 0 || !strings.ContainsRune(s, '\t') {
		return s
	}
	var out strings.Builder
	col := 0
	for _, r := range s {
		switch r {
		case '\t':
			next := col + tabWidth - col%tabWidth
			out.WriteString(strings.Repeat(" ", next-col))
			col = next
		default:
			out.WriteRune(r)
			col += runewidth.RuneWidth(r)
			if r == '\n' {
				col = 0
			}
		}
	}
	return out.String()
}

// containerLogCursor extracts the timestamp prefix of the newest non-empty
// line (docker Timestamps:true format, RFC3339Nano) for incremental fetches.
func containerLogCursor(content string) (time.Time, bool) {
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if line == "" {
			continue
		}
		ts := line
		if sp := strings.IndexByte(line, ' '); sp > 0 {
			ts = line[:sp]
		}
		t, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			continue
		}
		return t, true
	}
	return time.Time{}, false
}

// pruneLines keeps only the last n lines of s, used to bound the pre-wrapped
// log buffer after incremental appends.
func pruneLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// ---- navigation ----

// switchTab activates a tab and resets the selection to its top - the
// selected index is shared between tabs, so keeping it would place the
// highlight at an arbitrary row of the new list.
func (m *Model) switchTab(t tab) {
	m.activeTab = t
	m.selectedIdx = 0
	m.mainYOff = 0
	m.logSel = textSel{}
	m.dragSel = false
	m.detailSel = textSel{}
	m.detailDragSel = false
	m.fitViewports()
}

func (m *Model) moveUp() {
	if m.selectedIdx > 0 {
		m.selectedIdx--
	}
	m.scrollToSelected()
}

func (m *Model) moveDown() {
	maxIdx := len(m.containers) - 1
	switch m.activeTab {
	case tabImages:
		maxIdx = len(m.images) - 1
	case tabVolumes:
		maxIdx = len(m.volumes) - 1
	case tabNetworks:
		maxIdx = len(m.networks) - 1
	}
	if m.selectedIdx < maxIdx {
		m.selectedIdx++
	}
	m.scrollToSelected()
}

func (m *Model) scrollToSelected() {
	vh := m.mainViewport.Height
	if vh <= 0 {
		return
	}
	rowY := m.selectedIdx
	yOff := m.mainYOff
	if rowY <= 0 {
		m.mainYOff = 0
	} else if rowY < yOff {
		m.mainYOff = rowY
	} else if rowY >= yOff+vh-1 {
		m.mainYOff = rowY - vh + 2
	}
}

// ---- commands ----

func (m Model) refreshNow() tea.Cmd {
	m.loading = true
	return func() tea.Msg {
		switch m.activeTab {
		case tabImages:
			images, err := m.docker.ListImages(m.showAll)
			if err != nil {
				return errMsg{err}
			}
			return imageMsg(images)
		case tabVolumes:
			volumes, err := m.docker.ListVolumes()
			if err != nil {
				return errMsg{err}
			}
			sort.Slice(volumes, func(i, j int) bool {
				return volumes[i].Name < volumes[j].Name
			})
			return volumeMsg(volumes)
		case tabNetworks:
			networks, err := m.docker.ListNetworks()
			if err != nil {
				return errMsg{err}
			}
			sort.Slice(networks, func(i, j int) bool {
				return networks[i].Name < networks[j].Name
			})
			return networkMsg(networks)
		default:
			containers, err := m.docker.ListContainers(m.showAll)
			if err != nil {
				return errMsg{err}
			}
			return containerMsg(containers)
		}
	}
}

func (m Model) toggleContainer() tea.Cmd {
	if m.activeTab != tabContainers || m.selectedIdx >= len(m.containers) {
		return nil
	}
	c := m.containers[m.selectedIdx]
	return func() tea.Msg {
		var err error
		if c.State == "running" {
			err = m.docker.StopContainer(c.ID)
		} else {
			err = m.docker.StartContainer(c.ID)
		}
		if err != nil {
			return errMsg{err}
		}
		time.Sleep(500 * time.Millisecond)
		return m.refreshNow()()
	}
}

func (m Model) restartContainer() tea.Cmd {
	if m.activeTab != tabContainers || m.selectedIdx >= len(m.containers) {
		return nil
	}
	c := m.containers[m.selectedIdx]
	return func() tea.Msg {
		err := m.docker.RestartContainer(c.ID)
		if err != nil {
			return errMsg{err}
		}
		time.Sleep(500 * time.Millisecond)
		return m.refreshNow()()
	}
}

// logTail is how many last lines the Logs pane fetches.
const logTail = "1000"

func (m Model) loadContainerLogs() tea.Cmd {
	if m.activeTab != tabContainers || m.selectedIdx >= len(m.containers) {
		return nil
	}
	c := m.containers[m.selectedIdx]
	inc := m.logFetchPlan(c)
	// While the user is actively selecting or dragging, never let a resync
	// (stale cursor past logResyncGap) trigger a full tail reload mid-drag:
	// that would re-wrap the buffer and destroy the in-flight selection.
	// A since-based append is small because the pane re-fetches every tick.
	if (m.logSel.active || m.dragSel) && !m.containerLogLastTS.IsZero() {
		inc = true
	}
	return func() tea.Msg {
		var reader io.ReadCloser
		var err error
		if inc {
			reader, err = m.docker.ContainerLogsSince(c.ID, m.containerLogLastTS)
		} else {
			reader, err = m.docker.ContainerLogs(c.ID, logTail, false)
		}
		if err != nil {
			return silentErrMsg{err}
		}
		defer reader.Close()
		data, err := io.ReadAll(reader)
		if err != nil {
			return errMsg{err}
		}
		return containerLogMsg{id: c.ID, content: docker.StripDockerStreamHeaders(data), incremental: inc}
	}
}

// logFetchPlan decides whether the next log fetch for c can be an
// incremental since-based append or must be a full tail reload. A cursor
// applies only to the container whose ID produced it; anything else (new
// selection, list refresh, container recreated after compose down/up) starts
// over with a full tail. A stale cursor older than logResyncGap also forces
// a full reload to avoid unbounded since-based catch-up.
func (m Model) logFetchPlan(c docker.Container) bool {
	if m.containerLogID != c.ID {
		return false
	}
	if m.containerLogLastTS.IsZero() {
		return false
	}
	return time.Since(m.containerLogLastTS) <= logResyncGap
}

// autoRefreshLogs re-fetches logs on every refresh tick while the Logs
// sub-tab is showing, keeping them current.
func (m Model) autoRefreshLogs() tea.Cmd {
	if m.activeTab != tabContainers || m.activeSubTab != subTabLogs || m.selectedIdx >= len(m.containers) {
		return nil
	}
	return m.loadContainerLogs()
}
