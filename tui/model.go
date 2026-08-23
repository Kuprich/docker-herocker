package tui

import (
	"fmt"
	"io"
	"sort"
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

type Model struct {
	docker   *docker.Client

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

	spinner spinner.Model
	help    help.Model
	helpOn  bool
	ready   bool
	width   int
	height  int
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
		m.spinner.Tick,
	)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.logViewport = viewport.New(
			msg.Width-5,
			msg.Height-tabBarHeight-helpBarHeight-4,
		)
		m.logViewport.Style = BaseStyle
		m.mainViewport = viewport.New(
			msg.Width,
			msg.Height-tabBarHeight-helpBarHeight-1,
		)
		m.mainViewport.Style = BaseStyle
		m.ready = true

	case tea.KeyMsg:
		switch {
		case key.Matches(msg, keys.Quit):
			return m, tea.Quit
		case key.Matches(msg, keys.Help):
			m.helpOn = !m.helpOn
		case key.Matches(msg, keys.Tab):
			m.cyclePanel()
		case key.Matches(msg, keys.Up):
			m.moveUp()
		case key.Matches(msg, keys.Down):
			m.moveDown()
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
			}
		case key.Matches(msg, keys.One):
			m.activeTab = tabContainers
			return m, m.refreshNow()
		case key.Matches(msg, keys.Two):
			m.activeTab = tabImages
			return m, m.refreshNow()
		case key.Matches(msg, keys.Three):
			m.activeTab = tabVolumes
			return m, m.refreshNow()
		case key.Matches(msg, keys.Four):
			m.activeTab = tabNetworks
			return m, m.refreshNow()
		}

	case containerMsg:
		m.containers = msg
		m.loading = false
		if m.selectedIdx >= len(m.containers) {
			m.selectedIdx = 0
		}
		m.mainViewport.GotoTop()
		return m, m.refreshDelayed()

	case imageMsg:
		m.images = msg
		m.loading = false
		if m.selectedIdx >= len(m.images) {
			m.selectedIdx = 0
		}
		m.mainViewport.GotoTop()
		return m, m.refreshDelayed()

	case volumeMsg:
		m.volumes = msg
		m.loading = false
		if m.selectedIdx >= len(m.volumes) {
			m.selectedIdx = 0
		}
		m.mainViewport.GotoTop()
		return m, m.refreshDelayed()

	case networkMsg:
		m.networks = msg
		m.loading = false
		if m.selectedIdx >= len(m.networks) {
			m.selectedIdx = 0
		}
		m.mainViewport.GotoTop()
		return m, m.refreshDelayed()

	case logMsg:
		m.logContent = string(msg)
		m.logViewport.SetContent(m.logContent)
		m.logViewport.GotoBottom()

	case errMsg:
		m.err = msg.err
		m.loading = false

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}

	var cmd tea.Cmd
	m.mainViewport, cmd = m.mainViewport.Update(msg)
	m.logViewport, _ = m.logViewport.Update(msg)
	return m, cmd
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
	return lipgloss.Place(m.width, m.height,
		lipgloss.Top, lipgloss.Left,
		content,
		lipgloss.WithWhitespaceBackground(t.Background),
	)
}

// ---- render helpers ----

func (m Model) renderHelpBar() string {
	if m.helpOn {
		return HelpBarStyle.Width(m.width).Render(m.help.View(keys))
	}
	h := " 1-4  tabs  •  ↑/↓  navigate  •  Enter  logs  •  Space  start/stop  •  r  restart  •  a  all  •  ?  help"
	return HelpBarStyle.Width(m.width).Render(h)
}

func (m Model) renderTabBar() string {
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
	tabsContent = lipgloss.Place(m.width, 1, lipgloss.Left, lipgloss.Top, tabsContent,
		lipgloss.WithWhitespaceBackground(t.Background),
	)
	line := lipgloss.NewStyle().Background(t.Background).Foreground(t.Border).Render(strings.Repeat("─", m.width))
	return lipgloss.JoinVertical(lipgloss.Top, line, tabsContent, line)
}

func (m Model) renderMain() string {
	if m.activePanel == panelLogs {
		return m.renderLogView()
	}
	w := m.width
	h := m.height - tabBarHeight - helpBarHeight

	var content string
	if m.err != nil {
		content = MainPanelStyle.Width(w).Height(h).Render(errorStyle.Render(m.err.Error()))
	} else {
		switch m.activeTab {
		case tabImages:
			content = m.renderImageList(w, h)
		case tabVolumes:
			content = m.renderVolumeList(w, h)
		case tabNetworks:
			content = m.renderNetworkList(w, h)
		default:
			content = m.renderContainerList(w, h)
		}
	}
	m.mainViewport.Width = w
	m.mainViewport.Height = h
	m.mainViewport.SetContent(content)
	m.mainViewport.Style = BaseStyle
	return m.mainViewport.View()
}

func (m Model) renderContainerList(w, h int) string {
	colW := w
	if m.loading && len(m.containers) == 0 {
		return MainPanelStyle.Width(w).Height(h).Render(BaseStyle.Foreground(t.Muted).Render(" Waiting for Docker…"))
	}
	if len(m.containers) == 0 {
		return MainPanelStyle.Width(w).Height(h).Render(BaseStyle.Foreground(t.Muted).Render(" No containers found"))
	}
	hdr := fmt.Sprintf("     %-29s %-29s  %-27s  %-30s", "NAME", "STATUS", "IMAGE", "PORTS")
	hdr = hdr + strings.Repeat(" ", colW-len([]rune(hdr)))
	header := lipgloss.NewStyle().Background(t.Background).Foreground(t.Accent).Bold(true).Render(hdr)
	sep := lipgloss.NewStyle().Background(t.Background).Foreground(t.Muted).Render(strings.Repeat("─", colW))

	var rows []string
	for i, c := range m.containers {
		name := Truncate(strings.TrimPrefix(c.Names[0], "/"), 29)
		status := Truncate(c.Status, 29)
		ports := formatPorts(c.Ports)
		img := Truncate(c.Image, 27)

		dot := "●"
		dotColor := t.Muted
		switch c.State {
		case "running":
			dotColor = t.Success
		case "paused":
			dotColor = t.Warning
		case "exited":
			dotColor = t.Muted
			dot = "○"
		default:
			dotColor = t.Error
		}

		line := fmt.Sprintf(" %s  %-29s %-29s  %-27s  %-30s", dot, name, status, img, ports)
		runes := []rune(line)
		padding := colW - len(runes)
		if padding > 0 {
			line = line + strings.Repeat(" ", padding)
			runes = []rune(line)
		} else if padding < 0 {
			runes = runes[:max(colW, 3)]
			line = string(runes)
		}
		dotRune := string(runes[1:2])
		rest := string(runes[2:])

		bg := t.Background
		if i == m.selectedIdx {
			bg = lipgloss.Color("#1c2d1f")
		}
		bgStyle := lipgloss.NewStyle().Background(bg)
		row := bgStyle.Render(" ") +
			bgStyle.Copy().Foreground(dotColor).Render(dotRune) +
			bgStyle.Foreground(t.Foreground).Render(rest)
		rows = append(rows, row)
	}
	return MainPanelStyle.Width(w).Height(h).Render(
		lipgloss.JoinVertical(lipgloss.Top, header, sep, lipgloss.JoinVertical(lipgloss.Top, rows...)),
	)
}

func (m Model) renderImageList(w, h int) string {
	colW := w
	if m.loading && len(m.images) == 0 {
		return MainPanelStyle.Width(w).Height(h).Render(BaseStyle.Foreground(t.Muted).Render(" Loading images…"))
	}
	if len(m.images) == 0 {
		return MainPanelStyle.Width(w).Height(h).Render(BaseStyle.Foreground(t.Muted).Render(" No images found"))
	}

	hdr := fmt.Sprintf("     %-42s %-16s  %-20s  %-14s", "REPOSITORY:TAG", "IMAGE ID", "CREATED", "SIZE")
	padding := colW - len([]rune(hdr))
	if padding > 0 {
		hdr += strings.Repeat(" ", padding)
	}
	header := lipgloss.NewStyle().Background(t.Background).Foreground(t.Accent).Bold(true).Render(hdr)
	sep := lipgloss.NewStyle().Background(t.Background).Foreground(t.Muted).Render(strings.Repeat("─", colW))

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
			bg = lipgloss.Color("#1c2d1f")
		}
		bgStyle := lipgloss.NewStyle().Background(bg)
		row := bgStyle.Render(" ") +
			bgStyle.Foreground(t.Foreground).Render(string(runes[1:]))
		rows = append(rows, row)
	}
	return MainPanelStyle.Width(w).Height(h).Render(
		lipgloss.JoinVertical(lipgloss.Top, header, sep, lipgloss.JoinVertical(lipgloss.Top, rows...)),
	)
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

func (m Model) renderVolumeList(w, h int) string {
	colW := w
	if m.loading && len(m.volumes) == 0 {
		return MainPanelStyle.Width(w).Height(h).Render(BaseStyle.Foreground(t.Muted).Render(" Loading volumes…"))
	}
	if len(m.volumes) == 0 {
		return MainPanelStyle.Width(w).Height(h).Render(BaseStyle.Foreground(t.Muted).Render(" No volumes found"))
	}

	hdr := fmt.Sprintf("     %-24s %-16s  %-30s  %-14s", "NAME", "DRIVER", "MOUNTPOINT", "SCOPE")
	padding := colW - len([]rune(hdr))
	if padding > 0 {
		hdr += strings.Repeat(" ", padding)
	}
	header := lipgloss.NewStyle().Background(t.Background).Foreground(t.Accent).Bold(true).Render(hdr)
	sep := lipgloss.NewStyle().Background(t.Background).Foreground(t.Muted).Render(strings.Repeat("─", colW))

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
			bg = lipgloss.Color("#1c2d1f")
		}
		bgStyle := lipgloss.NewStyle().Background(bg)
		row := bgStyle.Render(" ") +
			bgStyle.Foreground(t.Foreground).Render(string(runes[1:]))
		rows = append(rows, row)
	}
	return MainPanelStyle.Width(w).Height(h).Render(
		lipgloss.JoinVertical(lipgloss.Top, header, sep, lipgloss.JoinVertical(lipgloss.Top, rows...)),
	)
}

func (m Model) renderNetworkList(w, h int) string {
	colW := w
	if m.loading && len(m.networks) == 0 {
		return MainPanelStyle.Width(w).Height(h).Render(BaseStyle.Foreground(t.Muted).Render(" Loading networks…"))
	}
	if len(m.networks) == 0 {
		return MainPanelStyle.Width(w).Height(h).Render(BaseStyle.Foreground(t.Muted).Render(" No networks found"))
	}

	hdr := fmt.Sprintf("     %-28s %-16s  %-16s  %-16s", "NAME", "DRIVER", "ID", "SCOPE")
	padding := colW - len([]rune(hdr))
	if padding > 0 {
		hdr += strings.Repeat(" ", padding)
	}
	header := lipgloss.NewStyle().Background(t.Background).Foreground(t.Accent).Bold(true).Render(hdr)
	sep := lipgloss.NewStyle().Background(t.Background).Foreground(t.Muted).Render(strings.Repeat("─", colW))

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
			bg = lipgloss.Color("#1c2d1f")
		}
		bgStyle := lipgloss.NewStyle().Background(bg)
		row := bgStyle.Render(" ") +
			bgStyle.Foreground(t.Foreground).Render(string(runes[1:]))
		rows = append(rows, row)
	}
	return MainPanelStyle.Width(w).Height(h).Render(
		lipgloss.JoinVertical(lipgloss.Top, header, sep, lipgloss.JoinVertical(lipgloss.Top, rows...)),
	)
}

func (m Model) renderLogView() string {
	w := m.width
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