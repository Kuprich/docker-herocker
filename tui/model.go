package tui

import (
	"fmt"
	"io"
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
)

type panel int

const (
	panelMain panel = iota
	panelLogs
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
type logMsg string
type containerLogMsg string
type detailErrMsg struct{ err error }
type containerDetailsMsg struct {
	id      string
	details *docker.ContainerDetails
}

type subTab int

const (
	subTabInfo subTab = iota
	subTabLogs
)

type Model struct {
	docker *docker.Client

	containers  []docker.Container
	images      []docker.Image
	volumes     []docker.Volume
	networks    []docker.Network
	selectedIdx int
	activePanel panel
	activeTab   tab
	showAll     bool
	loading     bool
	err         error

	logContent   string
	logViewport  viewport.Model
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
	containerLogContent  string
	containerLogViewport viewport.Model
	details              *docker.ContainerDetails
	detailsID            string
	detailViewport       viewport.Model
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
		selectedIdx: 0,
		activePanel: panelMain,
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.refreshNow(),
		m.loadContainerDetails(),
		m.spinner.Tick,
	)
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
		m.logViewport = viewport.New(
			cw-5,
			msg.Height-tabBarHeight-helpBarHeight-4,
		)
		m.logViewport.Style = BaseStyle
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
		case key.Matches(msg, keys.Tab):
			m.cyclePanel()
		case msg.String() == "left":
			if m.activeTab == tabContainers {
				m.activeSubTab = subTabInfo
				return m, m.loadContainerDetails()
			}
		case msg.String() == "right":
			if m.activeTab == tabContainers {
				m.activeSubTab = subTabLogs
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
		case key.Matches(msg, keys.ViewLogs):
			return m, m.handleViewLogs()
		case key.Matches(msg, keys.Back):
			if m.activePanel == panelLogs {
				m.activePanel = panelMain
			} else if m.activeTab == tabContainers && m.activeSubTab == subTabLogs {
				m.activeSubTab = subTabInfo
			}
		case key.Matches(msg, keys.One):
			m.activeTab = tabContainers
			m.mainYOff = 0
			m.fitViewports()
			return m, tea.Batch(m.refreshNow(), m.loadContainerDetails())
		case key.Matches(msg, keys.Two):
			m.activeTab = tabImages
			m.mainYOff = 0
			m.fitViewports()
			return m, m.refreshNow()
		case key.Matches(msg, keys.Three):
			m.activeTab = tabVolumes
			m.mainYOff = 0
			m.fitViewports()
			return m, m.refreshNow()
		case key.Matches(msg, keys.Four):
			m.activeTab = tabNetworks
			m.mainYOff = 0
			m.fitViewports()
			return m, m.refreshNow()
		}

	case containerMsg:
		m.containers = msg
		m.loading = false
		if m.selectedIdx >= len(m.containers) {
			m.selectedIdx = 0
		}
		m.fitViewports()
		m.scrollToSelected()
		return m, tea.Batch(m.refreshDelayed(), m.loadContainerDetails())

	case imageMsg:
		m.images = msg
		m.loading = false
		if m.selectedIdx >= len(m.images) {
			m.selectedIdx = 0
		}
		m.fitViewports()
		m.scrollToSelected()
		return m, m.refreshDelayed()

	case volumeMsg:
		m.volumes = msg
		m.loading = false
		if m.selectedIdx >= len(m.volumes) {
			m.selectedIdx = 0
		}
		m.fitViewports()
		m.scrollToSelected()
		return m, m.refreshDelayed()

	case networkMsg:
		m.networks = msg
		m.loading = false
		if m.selectedIdx >= len(m.networks) {
			m.selectedIdx = 0
		}
		m.fitViewports()
		m.scrollToSelected()
		return m, m.refreshDelayed()

	case logMsg:
		m.logContent = string(msg)
		m.logViewport.SetContent(m.logContent)
		m.logViewport.GotoBottom()

	case containerLogMsg:
		m.containerLogContent = string(msg)
		m.containerLogViewport.SetContent(m.containerLogContent)
		m.containerLogViewport.GotoBottom()

	case containerDetailsMsg:
		m.details = msg.details
		m.detailsID = msg.id
		m.loading = false
		m.fitDetailViewport()

	case detailErrMsg:
		// keep previously loaded details; inspect failures are non-fatal

	case errMsg:
		m.err = msg.err
		m.loading = false

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case tea.MouseMsg:
		if msg.Type == tea.MouseLeft {
			return m.handleClick(msg.X, msg.Y)
		}

		if m.activePanel == panelLogs {
			var cmd tea.Cmd
			m.logViewport, cmd = m.logViewport.Update(msg)
			return m, cmd
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
			m.logViewport, _ = m.logViewport.Update(msg)
			return m, cmd
		}
	}

	if _, isKey := msg.(tea.KeyMsg); !isKey {
		var cmd tea.Cmd
		m.mainViewport, cmd = m.mainViewport.Update(msg)
		m.logViewport, _ = m.logViewport.Update(msg)
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
	return lipgloss.Place(m.width, m.height,
		lipgloss.Top, lipgloss.Left,
		content,
		lipgloss.WithWhitespaceBackground(t.Background),
	)
}

// ---- render helpers ----

func (m Model) renderHelpBar() string {
	cw := innerW(m.width)
	if m.helpOn {
		return HelpBarStyle.Width(cw).Render(m.help.View(keys))
	}
	h := " 1-4  tabs  •  ↑/↓  navigate  •  Enter  logs  •  Space  start/stop  •  r  restart  •  a  all  •  ?  help"
	return HelpBarStyle.Width(cw).Render(h)
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
	if m.activePanel == panelLogs {
		return m.renderLogView()
	}
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
	tv := func(s string) string { return Truncate(s, valW) }

	var b strings.Builder
	// The " Info:" title is part of the scrollable content so it scrolls
	// away with the body.
	titleStyle := lipgloss.NewStyle().Foreground(t.Accent).Bold(true)
	b.WriteString(titleStyle.Render(" Info:") +
		lipgloss.NewStyle().Background(t.Background).
			Render(strings.Repeat(" ", max(w-len([]rune(" Info:")), 0))) + "\n")

	line := func(label, value string) {
		fmt.Fprintf(&b, "  %-10s %s\n", label+":", tv(value))
	}
	// fillToWidth appends theme-background spaces so a line containing
	// embedded ANSI resets still reaches the full block width - lipgloss
	// pads shorter lines with UNSTYLED spaces after the last reset, which
	// would show as default terminal background.
	fillToWidth := func(v string) string {
		used := 13 + valW // indent + label + gap + value width
		if rest := w - used; rest > 0 {
			return v + lipgloss.NewStyle().Background(t.Background).Render(strings.Repeat(" ", rest))
		}
		return v
	}
	// coloredLine renders the value in color when it fits, like colorize().
	coloredLine := func(label, plain string, color lipgloss.Color) {
		fmt.Fprintf(&b, "  %-10s %s\n", label+":", fillToWidth(colorize(plain, valW, color)))
	}

	line("Name", strings.TrimPrefix(c.Names[0], "/"))
	line("ID", shortID(c.ID))
	line("Image", c.Image)
	coloredLine("Status", c.Status, stateColor(c.State))
	coloredLine("State", c.State, stateColor(c.State))
	if cell, ok := renderPortsCell(c.Ports, valW, t.Background); ok {
		fmt.Fprintf(&b, "  %-10s %s\n", "Ports:", fillToWidth(cell))
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
		b.WriteString("\n  Loading details…")
		return b.String()
	}

	line("Exit code", strconv.Itoa(d.State.ExitCode))
	if d.State.Health != nil && d.State.Health.Status != "" {
		line("Health", d.State.Health.Status)
	}

	section := lipgloss.NewStyle().Foreground(t.Accent).Bold(true)
	rowStyle := lipgloss.NewStyle().Background(t.Background)
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
		for _, n := range names {
			fmt.Fprintf(&b, "  %-13s %s\n", Truncate(n, 13), tv(d.NetworkSettings.Networks[n].IPAddress))
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
			fmt.Fprintf(&b, "  %s -> %s (%s)\n", tv(mt.Destination), tv(src), mode)
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
			fmt.Fprintf(&b, "  %s=%s\n", tv(k), tv(d.Config.Labels[k]))
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
	header := lipgloss.JoinHorizontal(lipgloss.Top,
		rowStyle.Copy().Foreground(t.Accent).Bold(true).Render(" Logs: "),
		rowStyle.Copy().Foreground(t.Foreground).Bold(true).Render(name),
		rowStyle.Copy().Foreground(t.Muted).Render("   Esc back "),
		rowStyle.Render(strings.Repeat(" ", w-len([]rune(" Logs: "+name+"   Esc back ")))),
	)

	m.containerLogViewport.Width = w - 1              // viewport shares the pane with the scrollbar column
	m.containerLogViewport.Height = max(bottomH-3, 1) // header + separator + blank gap row
	m.containerLogViewport.SetContent(wrapText(m.containerLogContent, w-1))

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
}

// fitDetailViewport syncs the detail (Info) viewport's size and content on
// the persistent model, mirroring the bottom pane of the containers split.
func (m *Model) fitDetailViewport() {
	w := innerW(m.width)
	h := m.height - tabBarHeight - helpBarHeight
	topH := int(float64(h) * splitRatio)
	bottomH := h - topH - subTabBarHeight

	vh := bottomH - 2 // " Info:" header + a constant blank gap row at the bottom
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
			return detailErrMsg{err}
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
				m.activeTab = tab(i)
				m.mainYOff = 0
				m.fitViewports()
				return m, m.refreshNow()
			}
		}
		return m, nil
	}

	contentH := m.height - tabBarHeight - helpBarHeight
	topH := int(float64(contentH) * splitRatio)

	if m.activeTab == tabContainers {
		absY := y - tabBarHeight

		// Sub-tab bar area (3 rows: line + tabs + line)
		if absY >= topH && absY < topH+3 {
			m.activePanel = panelMain
			if absY == topH+1 {
				subItems := []string{"Info", "Logs"}
				cum := 0
				for i, item := range subItems {
					w := lipgloss.Width(SubTabInactiveStyle.Render(" " + item + " "))
					cum += w
					if x < cum {
						m.activeSubTab = subTab(i)
						if i == int(subTabLogs) {
							return m, m.loadContainerLogs()
						}
						return m, m.loadContainerDetails()
					}
				}
			}
			return m, nil
		}

		// Table rows
		if absY < topH {
			m.activePanel = panelMain
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
		m.activePanel = panelMain
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

func (m Model) renderLogView() string {
	w := innerW(m.width)
	h := m.height - tabBarHeight - helpBarHeight

	if len(m.containers) == 0 || m.selectedIdx >= len(m.containers) {
		return MainPanelStyle.Width(w).Height(h).Render("")
	}
	c := m.containers[m.selectedIdx]
	name := strings.TrimPrefix(c.Names[0], "/")

	header := BaseStyle.Copy().Foreground(t.Accent).Bold(true).Render(" Logs: ") +
		BaseStyle.Copy().Bold(true).Render(name) +
		BaseStyle.Copy().Foreground(t.Muted).Render("   Esc back ")

	content := lipgloss.JoinVertical(lipgloss.Top,
		header,
		m.logViewport.View(),
	)
	return MainPanelStyle.Width(w).Height(h).Render(content)
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

func wrapText(text string, width int) string {
	if width <= 0 {
		return text
	}
	var result strings.Builder
	for _, line := range strings.Split(text, "\n") {
		runes := []rune(line)
		for len(runes) > width {
			result.WriteString(string(runes[:width]))
			result.WriteByte('\n')
			runes = runes[width:]
		}
		result.WriteString(string(runes))
		result.WriteByte('\n')
	}
	return strings.TrimRight(result.String(), "\n")
}

// ---- navigation ----

func (m *Model) cyclePanel() {
	if m.activePanel == panelMain {
		m.activePanel = panelLogs
	} else {
		m.activePanel = panelMain
	}
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

func (m Model) refreshDelayed() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg {
		return m.refreshNow()()
	})
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

func (m Model) handleViewLogs() tea.Cmd {
	if m.activeTab != tabContainers || m.selectedIdx >= len(m.containers) {
		return nil
	}
	c := m.containers[m.selectedIdx]
	m.activePanel = panelLogs
	return func() tea.Msg {
		reader, err := m.docker.ContainerLogs(c.ID, "100", false)
		if err != nil {
			return errMsg{err}
		}
		defer reader.Close()
		data, err := io.ReadAll(reader)
		if err != nil {
			return errMsg{err}
		}
		return logMsg(docker.StripDockerStreamHeaders(data))
	}
}

func (m Model) loadContainerLogs() tea.Cmd {
	if m.activeTab != tabContainers || m.selectedIdx >= len(m.containers) {
		return nil
	}
	c := m.containers[m.selectedIdx]
	return func() tea.Msg {
		reader, err := m.docker.ContainerLogs(c.ID, "100", false)
		if err != nil {
			return errMsg{err}
		}
		defer reader.Close()
		data, err := io.ReadAll(reader)
		if err != nil {
			return errMsg{err}
		}
		return containerLogMsg(docker.StripDockerStreamHeaders(data))
	}
}
