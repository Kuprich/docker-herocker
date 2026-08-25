package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/kuri4/dockerherocker/docker"
)

// stripANSI removes SGR escape sequences so assertions can inspect visible
// text and layout only.
func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		if inEsc {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
			continue
		}
		if r == '\x1b' {
			inEsc = true
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func TestStateColor(tt *testing.T) {
	cases := map[string]lipgloss.Color{
		"running":    t.Success,
		"paused":     t.Warning,
		"restarting": t.Error,
		"dead":       t.Error,
		"exited":     t.Muted,
		"created":    t.Info,
		"removing":   t.Info,
	}
	for state, want := range cases {
		if got := stateColor(state); got != want {
			tt.Errorf("stateColor(%q) = %v, want %v", state, got, want)
		}
	}
}

func TestStateColorsAreDistinct(tt *testing.T) {
	colors := []lipgloss.Color{
		stateColor("running"), stateColor("paused"), stateColor("restarting"),
		stateColor("exited"), stateColor("created"),
	}
	seen := map[lipgloss.Color]bool{}
	for _, c := range colors {
		if seen[c] {
			tt.Errorf("duplicate color in state palette: %v", c)
		}
		seen[c] = true
	}
}

func TestRenderPortsCellFillsWidthWithBackground(tt *testing.T) {
	ports := []docker.Port{
		{PrivatePort: 80, PublicPort: 8080, Type: "tcp"},
		{PrivatePort: 53, PublicPort: 5353, Type: "udp"},
	}
	cell, ok := renderPortsCell(ports, 40, t.Background)
	if !ok {
		tt.Fatal("expected cell to fit")
	}
	plain := stripANSI(cell)
	if len([]rune(plain)) != 40 {
		tt.Errorf("cell width = %d, want 40 (padded with background)", len([]rune(plain)))
	}
	want := "8080->80/tcp, 5353->53/udp"
	if !strings.HasPrefix(plain, want) {
		tt.Errorf("cell = %q, want prefix %q", plain, want)
	}
	// protocol suffixes carry their own color styles
	udpStyle := lipgloss.NewStyle().Background(t.Background).Foreground(t.Warning).Render("/udp")
	tcpStyle := lipgloss.NewStyle().Background(t.Background).Foreground(t.Muted).Render("/tcp")
	for _, frag := range []string{udpStyle, tcpStyle} {
		if !strings.Contains(cell, frag) {
			tt.Errorf("cell missing styled segment %q", stripANSI(frag))
		}
	}
}

func TestRenderPortsCellFallbackWhenTooLong(tt *testing.T) {
	ports := make([]docker.Port, 0, 20)
	for i := 0; i < 20; i++ {
		ports = append(ports, docker.Port{
			PrivatePort: 1000 + i, PublicPort: 20000 + i, Type: "tcp",
		})
	}
	if _, ok := renderPortsCell(ports, 30, t.Background); ok {
		tt.Error("expected ok=false when ports exceed width")
	}
}

func TestColorizeTruncatesLongValuesPlainly(tt *testing.T) {
	got := colorize(strings.Repeat("x", 50), 10, t.Success)
	plain := stripANSI(got)
	if len([]rune(plain)) > 10 {
		tt.Errorf("colorize did not truncate: %d runes", len([]rune(plain)))
	}
}

func TestWheelOverTableMovesSelectionLikeKeys(tt *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.containers = makeTestContainers(9)
	m.fitViewports()

	wheelDown := tea.MouseMsg{X: 20, Y: 8, Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress}
	keyJ := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")}

	wheelModel := testMouseUpdate(m, wheelDown)
	keyModel := testMouseUpdate(m, keyJ)

	if wheelModel.selectedIdx != keyModel.selectedIdx {
		tt.Errorf("wheel selection = %d, key selection = %d", wheelModel.selectedIdx, keyModel.selectedIdx)
	}
	if wheelModel.selectedIdx != 1 {
		tt.Errorf("selection = %d, want 1", wheelModel.selectedIdx)
	}
}

func testMouseUpdate(m Model, msg tea.Msg) Model {
	next, _ := m.Update(msg)
	return next.(Model)
}

func TestClickInLeftMarginIsIgnored(tt *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.containers = makeTestContainers(3)
	m.selectedIdx = 1
	m.fitViewports()

	click := tea.MouseMsg{X: 0, Y: 10, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}
	next := testMouseUpdate(m, click)
	if next.selectedIdx != 1 {
		tt.Errorf("margin click changed selection to %d", next.selectedIdx)
	}

	click = tea.MouseMsg{X: 5 - appMarginX, Y: 10, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}
	next = testMouseUpdate(m, click)
	if next.selectedIdx != 1 {
		tt.Errorf("content x=4 resolved to wrong row %d", next.selectedIdx)
	}
}

func TestViewRespectsHorizontalMargins(tt *testing.T) {
	m := New(nil)
	m.width = 100
	m.height = 24
	m.ready = true
	m.containers = makeTestContainers(2)
	m.fitViewports()

	for _, line := range strings.Split(m.View(), "\n") {
		plain := stripANSI(line)
		trimmed := strings.TrimRight(plain, " ")
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(plain, " ") {
			tt.Errorf("line does not start with left margin: %q", plain[:min(20, len(plain))])
		}
		break // checking the first content line is enough for the left side
	}
}

func TestContainerListRowColumnOffsets(tt *testing.T) {
	m := New(nil)
	m.width = 140
	m.height = 30
	m.ready = true
	m.loading = false
	m.containers = makeTestContainers(3)
	m.fitMainViewport()

	hdr, rows := m.renderContainerList(innerW(m.width), innerW(m.width)-1, m.height-tabBarHeight-helpBarHeight)
	_ = hdr
	plain := stripANSI(rows)
	lines := strings.Split(plain, "\n")
	for i, line := range lines {
		state := m.containers[i].State
		runes := []rune(line)
		idx := -1
		for j := range runes {
			if j+1 <= len(runes) && strings.HasPrefix(string(runes[j:]), state) {
				idx = j
				break
			}
		}
		if idx != stateCol {
			tt.Errorf("row %d: state at col %d, want %d (%q)", i, idx, stateCol, string(runes[:min(50, len(runes))]))
		}
	}
}

func TestTruncateHandlesMultibyteSafely(tt *testing.T) {
	s := "привет мир это длинная строка"
	got := Truncate(s, 10)
	if got == "" {
		tt.Fatal("empty truncation result")
	}
}

// makeTestContainers builds n synthetic running containers.
func makeTestContainers(n int) []docker.Container {
	out := make([]docker.Container, 0, n)
	for i := 0; i < n; i++ {
		states := []string{"running", "exited", "restarting"}
		out = append(out, docker.Container{
			ID:      strings.Repeat(string(rune('a'+i)), 12),
			Names:   []string{"/test-container-" + string(rune('0'+i))},
			Image:   "img:latest",
			State:   states[i%len(states)],
			Status:  "Up 5 minutes",
			Created: 1700000000,
			Command: "sleep infinity",
			Ports: []docker.Port{
				{PrivatePort: 8000 + i, PublicPort: 9000 + i, Type: "tcp"},
				{PrivatePort: 8000 + i, PublicPort: 9000 + i, Type: "tcp"}, // IPv6 dup
			},
		})
	}
	return out
}

func TestBuildDetailContentColoredLinesFillWidth(tt *testing.T) {
	m := New(nil)
	w := 80
	m.containers = makeTestContainers(1)
	out := m.buildDetailContent(w)
	for _, line := range strings.Split(out, "\n") {
		plain := stripANSI(line)
		isColoredField := strings.HasPrefix(strings.TrimSpace(plain), "Status:") ||
			strings.HasPrefix(strings.TrimSpace(plain), "State:") ||
			strings.HasPrefix(strings.TrimSpace(plain), "Ports:")
		if !isColoredField {
			continue
		}
		if got := len([]rune(plain)); got != w {
			tt.Errorf("colored line visible width = %d, want %d (%q)", got, w, plain)
		}
	}
}

func makeTestImages(n int) []docker.Image {
	out := make([]docker.Image, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, docker.Image{
			ID:       strings.Repeat(string(rune('a'+i)), 12),
			RepoTags: []string{"img" + string(rune('0'+i)) + ":latest"},
			Created:  1700000000,
			Size:     1000,
		})
	}
	return out
}

func TestSwitchTabResetsSelection(tt *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.containers = makeTestContainers(3)
	m.images = makeTestImages(14)
	m.selectedIdx = 12 // предпоследняя запись Images
	m.activeTab = tabImages
	m.fitViewports()

	keyThree := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("3")}
	next := testMouseUpdate(m, keyThree)

	if next.activeTab != tabVolumes {
		tt.Fatalf("activeTab = %d, want volumes", next.activeTab)
	}
	if next.selectedIdx != 0 {
		tt.Errorf("selectedIdx after switch = %d, want 0 (start of volumes list)", next.selectedIdx)
	}
}

func TestWheelAfterTabSwitchStartsFromTop(tt *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.images = makeTestImages(14)
	m.switchTab(tabImages)

	wheelDown := tea.MouseMsg{X: 20, Y: 8, Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress}
	next := testMouseUpdate(m, wheelDown)
	if next.selectedIdx != 1 {
		tt.Errorf("first wheel after switch moved selection to %d, want 1", next.selectedIdx)
	}
}

func TestImageMsgKeepsSelectionOnActiveImagesTab(tt *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.activeTab = tabImages
	m.images = makeTestImages(14)
	m.selectedIdx = 7
	m.fitViewports()

	next := testMouseUpdate(m, imageMsg(makeTestImages(14)))
	if next.selectedIdx != 7 {
		tt.Errorf("refresh reset selection to %d, want 7", next.selectedIdx)
	}
}

func TestStaleForeignMsgDoesNotResetWhenInRange(tt *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.activeTab = tabNetworks
	m.networks = []docker.Network{
		{ID: "a", Name: "bridge"}, {ID: "b", Name: "host"},
	}
	m.selectedIdx = 1
	m.fitViewports()

	// stale containers delivery from a parallel chain
	next := testMouseUpdate(m, containerMsg(makeTestContainers(9)))
	if next.selectedIdx != 1 {
		tt.Errorf("stale containerMsg reset selection to %d, want 1", next.selectedIdx)
	}
}

func TestContainerLogMsgFollowBehavior(tt *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.containers = makeTestContainers(1)
	m.containerLogViewport = viewport.New(80, 5)

	// first load on an empty viewport: must jump to bottom
	next := testMouseUpdate(m, containerLogMsg(strings.Repeat("line\n", 30)))
	if got := next.containerLogViewport.YOffset; got != 25 { // wrapText trims trailing newline: 30 lines - 5 height
		tt.Errorf("initial load YOffset = %d, want 26 (bottom)", got)
	}

	// user scrolled up to read history: refresh must keep their position
	scrolled := next
	scrolled.containerLogViewport.YOffset = 0
	next2 := testMouseUpdate(scrolled, containerLogMsg(strings.Repeat("line\n", 35)))
	if got := next2.containerLogViewport.YOffset; got != 0 {
		tt.Errorf("YOffset after refresh while reading = %d, want 0 (position preserved)", got)
	}

	// back to bottom: refresh follows the tail again
	back := next2
	back.containerLogViewport.YOffset = 31 // 36 lines - 5 height = bottom
	next3 := testMouseUpdate(back, containerLogMsg(strings.Repeat("line\n", 40)))
	if got := next3.containerLogViewport.YOffset; got != 35 { // 40 lines - 5 height
		tt.Errorf("follow YOffset = %d, want 36 (bottom)", got)
	}
}

func TestAutoRefreshLogsGuards(tt *testing.T) {
	m := New(nil)
	m.containers = makeTestContainers(1)
	m.activeTab = tabImages
	if cmd := m.autoRefreshLogs(); cmd != nil {
		tt.Error("expected nil cmd on non-containers tab")
	}
	m.activeTab = tabContainers
	m.activeSubTab = subTabInfo
	if cmd := m.autoRefreshLogs(); cmd != nil {
		tt.Error("expected nil cmd when Info sub-tab active")
	}
	m.activeSubTab = subTabLogs
	if cmd := m.autoRefreshLogs(); cmd == nil {
		tt.Error("expected cmd when Logs sub-tab active")
	}
}

func TestSuccessfulRefreshClearsError(tt *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.activeTab = tabImages
	m.images = makeTestImages(5)
	m.err = fmt.Errorf("No such container: deadbeef")
	m.fitViewports()

	next := testMouseUpdate(m, imageMsg(makeTestImages(5)))
	if next.err != nil {
		tt.Errorf("successful refresh did not clear m.err: %v", next.err)
	}
}
