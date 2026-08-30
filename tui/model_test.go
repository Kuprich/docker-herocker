package tui

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/kuri4/dockerherocker/docker"
	"github.com/mattn/go-runewidth"
	"github.com/muesli/termenv"
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
	// lipgloss downgrades to the Ascii profile when stdout is not a TTY;
	// force TrueColor so the emitted SGR sequences assert the real palette.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	tt.Cleanup(func() { lipgloss.SetColorProfile(prev) })

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
	// published host ports carry their own orange highlight
	hot := lipgloss.Color("#f5a742")
	for _, digit := range []string{"8080", "5353"} {
		frag := lipgloss.NewStyle().Background(t.Background).Foreground(hot).Render(digit)
		if !strings.Contains(cell, frag) {
			tt.Errorf("cell missing styled host port %q", digit)
		}
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

func TestRenderPortsCellPrivateOnlyUnhighlighted(tt *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	tt.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	cell, ok := renderPortsCell([]docker.Port{{PrivatePort: 80, Type: "tcp"}}, 20, t.Background)
	if !ok {
		tt.Fatal("expected single private port to fit")
	}
	plain := stripANSI(cell)
	if !strings.HasPrefix(plain, "80/tcp") {
		tt.Errorf("cell = %q, want prefix %q", plain, "80/tcp")
	}
	hot := lipgloss.NewStyle().Background(t.Background).Foreground(lipgloss.Color("#f5a742")).Render("80")
	if strings.Contains(cell, hot) {
		tt.Error("private-only port must not get the host highlight")
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

// openMenuFor raises the container context menu via the x key for the
// selected index.
func openMenuFor(m Model, idx int) Model {
	m.selectedIdx = idx
	return testMouseUpdate(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
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
	m.logFollow = false
	m.containers = makeTestContainers(1)
	m.containerLogViewport = viewport.New(80, 5)

	// first load on an empty viewport: full tail replace must jump to bottom
	next := testMouseUpdate(m, logMsg(m, strings.Repeat("line\n", 30), false))
	if got := next.containerLogViewport.YOffset; got != 25 { // wrapText trims trailing newline: 30 lines - 5 height
		tt.Errorf("initial load YOffset = %d, want 26 (bottom)", got)
	}

	// user scrolled up to read history: incremental refresh must keep position
	scrolled := next
	scrolled.containerLogViewport.YOffset = 0
	next2 := testMouseUpdate(scrolled, logMsg(scrolled, strings.Repeat("line\n", 35), true))
	if got := next2.containerLogViewport.YOffset; got != 0 {
		tt.Errorf("YOffset after refresh while reading = %d, want 0 (position preserved)", got)
	}

	// back to bottom: refresh follows the tail again
	back := next2
	back.containerLogViewport.YOffset = 60 // 65 lines - 5 height = bottom
	next3 := testMouseUpdate(back, logMsg(back, strings.Repeat("line\n", 40), true))
	if got := next3.containerLogViewport.YOffset; got != 100 { // 105 lines - 5 height
		tt.Errorf("follow YOffset = %d, want 101 (bottom)", got)
	}
}

func TestContainerLogMsgFollowForced(tt *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.logFollow = true // checkbox on: always chase the tail
	m.containers = makeTestContainers(1)
	m.containerLogViewport = viewport.New(80, 5)

	next := testMouseUpdate(m, logMsg(m, strings.Repeat("line\n", 30), false))
	// reading at the very top, new lines arrive: follow must still snap to bottom
	next.containerLogViewport.YOffset = 0
	next2 := testMouseUpdate(next, logMsg(next, strings.Repeat("line\n", 40), true))
	if got := next2.containerLogViewport.YOffset; got != 65 { // 70 lines - 5 height
		tt.Errorf("forced follow YOffset = %d, want 65 (bottom)", got)
	}
}

// logMsg builds a containerLogMsg for the model's selected container.
func logMsg(m Model, content string, incremental bool) containerLogMsg {
	return containerLogMsg{id: m.containers[m.selectedIdx].ID, content: content, incremental: incremental}
}

func TestLogSelectionSurvivesFollowTick(tt *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.logFollow = true
	m.containers = makeTestContainers(1)
	m.containerLogViewport = viewport.New(80, 5)

	// back at the bottom with a live selection in the middle of the buffer
	first := testMouseUpdate(m, logMsg(m, strings.Repeat("line\n", 30), false))
	if got := first.containerLogViewport.YOffset; got != 25 {
		tt.Fatalf("initial YOffset = %d, want 25 (bottom)", got)
	}
	held := first
	held.logSel = textSel{active: true, anR: 26, anC: 0, endR: 27, endC: 3}

	// new lines arrive while the selection is active: follow must freeze
	next := testMouseUpdate(held, logMsg(held, strings.Repeat("line\n", 10), true))
	if !next.logSel.active || next.logSel.anR != 26 || next.logSel.endR != 27 {
		tt.Errorf("selection damaged by incoming lines: %+v", next.logSel)
	}
	if got := next.containerLogViewport.YOffset; got != 25 {
		tt.Errorf("YOffset with active selection = %d, want 25 (frozen, not 35)", got)
	}

	// clearing the selection lets follow chase the tail again
	released := next
	released.logSel = textSel{}
	next2 := testMouseUpdate(released, logMsg(released, strings.Repeat("line\n", 10), true))
	if got := next2.containerLogViewport.YOffset; got != 45 { // 50 lines - 5 height
		tt.Errorf("follow after clear YOffset = %d, want 45 (bottom)", got)
	}
}

func TestLogSelectionSurvivesFollowTickAtBottom(tt *testing.T) {
	// checkbox off, but the user is reading at the bottom: AtBottom() alone
	// must not shove a live selection off-screen either.
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.logFollow = false
	m.containers = makeTestContainers(1)
	m.containerLogViewport = viewport.New(80, 5)

	first := testMouseUpdate(m, logMsg(m, strings.Repeat("line\n", 30), false))
	if got := first.containerLogViewport.YOffset; got != 25 {
		tt.Fatalf("initial YOffset = %d, want 25 (bottom)", got)
	}
	held := first
	held.logSel = textSel{active: true, anR: 26, anC: 0, endR: 27, endC: 3}
	next := testMouseUpdate(held, logMsg(held, strings.Repeat("line\n", 10), true))
	if !next.logSel.active {
		tt.Error("AtBottom-implied follow must respect an active selection")
	}
	if got := next.containerLogViewport.YOffset; got != 25 {
		tt.Errorf("YOffset with active selection at bottom = %d, want 25", got)
	}
}

func TestLogInFlightDragFreezesFollow(tt *testing.T) {
	// an in-flight drag (no released selection yet) must not get yanked
	// by an incoming log tick mid-motion.
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.logFollow = true
	m.containers = makeTestContainers(1)
	m.containerLogViewport = viewport.New(80, 5)

	first := testMouseUpdate(m, logMsg(m, strings.Repeat("line\n", 30), false))
	dragging := first
	dragging.dragSel = true
	dragging.logSel = textSel{anR: 26, anC: 0, endR: 27, endC: 3}
	next := testMouseUpdate(dragging, logMsg(dragging, strings.Repeat("line\n", 10), true))
	if got := next.containerLogViewport.YOffset; got != 25 {
		tt.Errorf("YOffset during in-flight drag = %d, want 25 (frozen)", got)
	}
}

func TestContainerLogMsgIncrementalAppends(tt *testing.T) {
	id := "aaaaaaaaaaaa"
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.containers = makeTestContainers(1)
	m.containerLogViewport = viewport.New(80, 5)
	base, _ := time.Parse(time.RFC3339Nano, "2026-08-28T13:07:03.000000001Z")
	m.containerLogID = id
	m.containerLogLastTS = base

	msg := containerLogMsg{
		id:          id,
		content:     "2026-08-28T13:07:03.100000001Z alpha\n2026-08-28T13:07:03.200000001Z beta\n",
		incremental: true,
	}
	next := testMouseUpdate(m, msg)
	if !strings.Contains(next.containerLogContent, "alpha") || !strings.Contains(next.containerLogContent, "beta") {
		tt.Errorf("incremental content not appended: %q", next.containerLogContent)
	}
	wantTS, _ := time.Parse(time.RFC3339Nano, "2026-08-28T13:07:03.200000001Z")
	if !next.containerLogLastTS.Equal(wantTS) {
		tt.Errorf("cursor = %v, want %v", next.containerLogLastTS, wantTS)
	}
	if next.containerLogID != id {
		tt.Errorf("containerLogID = %q, want %q", next.containerLogID, id)
	}
}

func TestContainerLogMsgStaleIDIgnored(tt *testing.T) {
	m := New(nil)
	m.containers = makeTestContainers(2)
	m.selectedIdx = 0
	next := testMouseUpdate(m, containerLogMsg{
		id:          m.containers[1].ID, // different container
		content:     "WRONG CONTENT",
		incremental: false,
	})
	if next.containerLogContent != "" {
		tt.Errorf("stale container log replaced content: %q", next.containerLogContent)
	}
}

func TestLogFetchPlan(tt *testing.T) {
	m := New(nil)
	c := docker.Container{ID: "c1"}

	if m.logFetchPlan(c) {
		tt.Error("fresh model must do a full fetch")
	}

	m.containerLogID = "c1"
	if m.logFetchPlan(c) {
		tt.Error("matching ID but zero cursor must do a full fetch")
	}

	m.containerLogLastTS = time.Now()
	if !m.logFetchPlan(c) {
		tt.Error("matching ID with fresh cursor must do an incremental fetch")
	}

	m.containerLogLastTS = time.Now().Add(-10 * time.Second)
	if m.logFetchPlan(c) {
		tt.Error("stale cursor past logResyncGap must do a full fetch")
	}

	m.containerLogLastTS = time.Now()
	m.containerLogID = "other"
	if m.logFetchPlan(c) {
		tt.Error("cursor belongs to a different container, must do a full fetch")
	}
}

func TestContainerLogCursor(tt *testing.T) {
	ts, ok := containerLogCursor("2026-08-28T13:07:03.100000001Z a\n2026-08-28T13:07:03.200000001Z b\n")
	if !ok {
		tt.Fatal("expected parseable cursor")
	}
	want, _ := time.Parse(time.RFC3339Nano, "2026-08-28T13:07:03.200000001Z")
	if !ts.Equal(want) {
		tt.Errorf("cursor = %v, want %v", ts, want)
	}

	if _, ok := containerLogCursor("no timestamp at all\nmore text\n"); ok {
		tt.Error("unparseable content must report ok=false")
	}
	if _, ok := containerLogCursor(""); ok {
		tt.Error("empty content must report ok=false")
	}
}

func TestFollowToggleKey(tt *testing.T) {
	m := New(nil)
	m.activeTab = tabContainers
	m.activeSubTab = subTabLogs
	m.containers = makeTestContainers(1)

	// default is on
	if !m.logFollow {
		tt.Fatal("default logFollow = false, want true")
	}

	next := testMouseUpdate(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	if next.logFollow {
		tt.Error("key 'f' did not toggle logFollow off")
	}
	next2 := testMouseUpdate(next, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	if !next2.logFollow {
		tt.Error("key 'f' did not toggle logFollow back on")
	}
}

func TestFollowToggleClick(tt *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.activeTab = tabContainers
	m.activeSubTab = subTabLogs
	m.containers = makeTestContainers(1)

	contentH := m.height - tabBarHeight - helpBarHeight
	topH := int(float64(contentH) * splitRatio)
	start, _ := m.followCheckboxCols()
	click := tea.MouseMsg{Type: tea.MouseLeft, X: appMarginX + start, Y: topH + 3 + tabBarHeight}

	next := testMouseUpdate(m, click)
	if next.logFollow {
		tt.Error("click on checkbox did not toggle logFollow off")
	}
}

func TestMouseModifiersAreIgnored(tt *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.activeTab = tabContainers
	m.activeSubTab = subTabLogs
	m.containers = makeTestContainers(5)
	m.containerLogViewport.SetContent(strings.Repeat("log line\n", 50))
	m.containerLogViewport.Height = 10
	m.containerLogViewport.YOffset = 3
	m.selectedIdx = 1

	// Without the modifier this click would select table row 2.
	shiftClick := tea.MouseMsg{
		Type: tea.MouseLeft, X: appMarginX + 10, Y: tabBarHeight + 4, Shift: true,
	}
	next := testMouseUpdate(m, shiftClick)
	if next.selectedIdx != 1 {
		tt.Errorf("shift click changed selectedIdx to %d", next.selectedIdx)
	}
	if !next.logFollow {
		tt.Error("shift click toggled logFollow")
	}

	// Without the modifier this drag would scroll the log viewport.
	contentH := m.height - tabBarHeight - helpBarHeight
	topH := int(float64(contentH) * splitRatio)
	shiftDrag := tea.MouseMsg{
		X: 50, Y: tabBarHeight + topH + 2, Button: tea.MouseButtonLeft,
		Action: tea.MouseActionMotion, Shift: true,
	}
	next = testMouseUpdate(next, shiftDrag)
	if next.containerLogViewport.YOffset != 3 {
		tt.Errorf("shift drag scrolled viewport to YOffset %d", next.containerLogViewport.YOffset)
	}

	ctrlClick := tea.MouseMsg{
		Type: tea.MouseLeft, X: appMarginX + 10, Y: tabBarHeight + 4, Ctrl: true,
	}
	next = testMouseUpdate(next, ctrlClick)
	if next.selectedIdx != 1 {
		tt.Errorf("ctrl click changed selectedIdx to %d", next.selectedIdx)
	}
}

func TestFollowCheckboxGlyph(tt *testing.T) {
	on := New(nil)
	on.logFollow = true
	off := New(nil)
	off.logFollow = false
	if got := on.followCheckboxGlyph(); got != "●" {
		tt.Errorf("on glyph = %q, want %q", got, "●")
	}
	if got := off.followCheckboxGlyph(); got != "○" {
		tt.Errorf("off glyph = %q, want %q", got, "○")
	}
	if got := on.followCheckboxText(); got != "● follow" {
		tt.Errorf("on text = %q, want %q", got, "● follow")
	}
	if got := off.followCheckboxText(); got != "○ follow" {
		tt.Errorf("off text = %q, want %q", got, "○ follow")
	}
}

func TestFollowDotColors(tt *testing.T) {
	// lipgloss downgrades to the Ascii profile when stdout is not a TTY;
	// force TrueColor so the emitted SGR sequences assert the real palette.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	tt.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	on := New(nil)
	on.width = 120
	on.activeSubTab = subTabLogs
	on.containers = makeTestContainers(1)
	on.logFollow = true
	raw := on.renderSubLogView(100, 10)
	if !regexp.MustCompile(`\x1b\[[0-9;]*38;2;63;185;80[0-9;]*m●`).MatchString(raw) {
		tt.Errorf("follow ON circle not accent-green (63;185;80):\n%s", raw)
	}

	off := New(nil)
	off.width = 120
	off.activeSubTab = subTabLogs
	off.containers = makeTestContainers(1)
	off.logFollow = false
	rawOff := off.renderSubLogView(100, 10)
	if !regexp.MustCompile(`\x1b\[[0-9;]*38;2;139;147;158[0-9;]*m○`).MatchString(rawOff) {
		tt.Errorf("follow OFF circle not muted-gray (139;147;158):\n%s", rawOff)
	}
}

func TestLogHeaderRendersCheckbox(tt *testing.T) {
	on := New(nil)
	on.width = 120
	on.containers = makeTestContainers(1)
	on.activeSubTab = subTabLogs
	on.logFollow = true
	if out := stripANSI(on.renderSubLogView(100, 10)); !strings.Contains(out, "● follow") {
		tt.Errorf("header with follow ON missing '● follow':\n%s", out)
	}

	off := New(nil)
	off.width = 120
	off.containers = makeTestContainers(1)
	off.activeSubTab = subTabLogs
	off.logFollow = false
	if out := stripANSI(off.renderSubLogView(100, 10)); !strings.Contains(out, "○ follow") {
		tt.Errorf("header with follow OFF missing '○ follow':\n%s", out)
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

func TestBuildDetailContentNetworksNameColumn(tt *testing.T) {
	m := New(nil)
	w := 80
	m.containers = makeTestContainers(1)
	m.detailsID = m.containers[0].ID
	m.details = &docker.ContainerDetails{}
	m.details.NetworkSettings.Networks = map[string]docker.NetworkEndpoint{
		"t1_test_default":                 {IPAddress: "172.19.0.9"},
		"dockerherocker_frontend_network": {IPAddress: "172.20.0.2"},
	}
	out := m.buildDetailContent(w)
	for _, want := range []string{"t1_test_default", "dockerherocker_frontend_network", "172.19.0.9", "172.20.0.2"} {
		if !strings.Contains(stripANSI(out), want) {
			tt.Errorf("network line missing %q in output:\n%s", want, stripANSI(out))
		}
	}
	if strings.Contains(stripANSI(out), "…") {
		tt.Errorf("long network name got truncated at w=%d:\n%s", w, stripANSI(out))
	}
}

func TestBuildDetailContentMultilineLabelStaysOneRow(tt *testing.T) {
	for _, w := range []int{60, 80, 120, 127, 160} {
		m := New(nil)
		m.containers = makeTestContainers(1)
		m.detailsID = m.containers[0].ID
		m.details = &docker.ContainerDetails{}
		m.details.Config.Labels = map[string]string{
			"com.example.collapse":           "A\n\nB C D E F G",
			"com.example.verylong":           strings.Repeat("x", 1000),
			"org.opencontainers.description": "The Ubuntu container image maintained by Canonical\n\nUbuntu is a Debian-based Linux operating system that runs from the desktop to the cloud.",
		}
		out := m.buildDetailContent(w)
		plain := stripANSI(out)
		rows := strings.Split(plain, "\n")
		// Styled and plain content must carry the same row count: a value with
		// embedded newlines must never split into extra rows (each would be
		// completed by the viewport with unstyled whitespace).
		if got := len(strings.Split(out, "\n")); got != len(rows) {
			tt.Errorf("w=%d: styled/plain row count mismatch: %d vs %d", w, got, len(rows))
		}
		labelRows := map[string]string{}
		for _, row := range rows {
			if lipgloss.Width(row) > w {
				tt.Errorf("w=%d: content row wider than pane (%d > %d): %q", w, lipgloss.Width(row), w, row)
			}
			for _, key := range []string{"com.example.collapse", "com.example.verylong", "org.opencontainers.description"} {
				if strings.Contains(row, key) {
					labelRows[key] = row
				}
			}
		}
		if row := labelRows["com.example.collapse"]; row == "" {
			tt.Errorf("w=%d: collapse label missing", w)
		} else if !strings.Contains(row, "A  B C D E F G") {
			tt.Errorf("w=%d: newlines not collapsed: %q", w, row)
		}
		if row := labelRows["com.example.verylong"]; row == "" {
			tt.Errorf("w=%d: verylong label missing", w)
		} else if lipgloss.Width(row) != w {
			tt.Errorf("w=%d: verylong row width %d, want full pane width %d: %q", w, lipgloss.Width(row), w, row)
		} else if !strings.HasSuffix(row, "…") {
			tt.Errorf("w=%d: clamped row missing ellipsis: %q", w, row)
		}
		if row := labelRows["org.opencontainers.description"]; row == "" {
			tt.Errorf("w=%d: description label missing", w)
		} else if lipgloss.Width(row) > w {
			tt.Errorf("w=%d: description row wider than pane: %q", w, row)
		}
	}
}

func TestCutPlainTrimsToWidth(tt *testing.T) {
	got := cutPlain("some\x1b[38;2;1;2;3m styled\x1b[0m text", 6)
	if got != "some s" {
		tt.Errorf("cutPlain = %q, want %q", got, "some s")
	}
	if got := cutPlain("short", 6); got != "short" {
		tt.Errorf("cutPlain no-op expected, got %q", got)
	}
	if got := cutPlain("ab", 1); got != "a" {
		tt.Errorf("cutPlain = %q, want %q", got, "a")
	}
}

// logTestModel returns a Model wired for Logs-pane selection tests: width 120,
// height 30 (so the content band starts at screen y == tabBarHeight+topH+5)
// with a 6-line buffer scrolled one row in.
func logTestModel() Model {
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.activeTab = tabContainers
	m.activeSubTab = subTabLogs
	m.containers = makeTestContainers(1)
	m.logFollow = false
	m.containerLogContent = "alpha\nbeta\ngamma\ndelta\nepsilon\nzeta"
	m.containerLogViewport.SetContent(m.containerLogContent)
	m.containerLogViewport.Height = 5
	m.containerLogViewport.YOffset = 1
	return m
}

func detailTestModel() Model {
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.activeTab = tabContainers
	m.activeSubTab = subTabInfo
	m.containers = makeTestContainers(1)
	m.detailsID = m.containers[0].ID
	m.details = &docker.ContainerDetails{}
	m.details.State.Status = "running"
	m.fitViewports()
	return m
}

func TestInfoDragSelectsAndCopies(tt *testing.T) {
	m := detailTestModel()
	if m.detailContent == "" {
		tt.Fatal("detail content not built")
	}
	// Info content band starts right under the sub-tab strip: screen y=19 is
	// buffer row 0 (" Info:"), y=20 -> row 1, y=22 -> row 3.
	press := tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 20}
	next := testMouseUpdate(m, press)
	if !next.detailDragSel {
		tt.Fatal("press in the Info body should start a drag")
	}
	next = testMouseUpdate(next, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, X: 30, Y: 22})
	if next.detailSel.endR != 3 {
		tt.Errorf("Info drag end row = %d, want 3", next.detailSel.endR)
	}
	if next.detailSel.endC < 0 || next.detailSel.endC >= len([]rune(strings.Split(next.detailContent, "\n")[3])) {
		tt.Errorf("Info drag end col = %d out of row bounds", next.detailSel.endC)
	}
	next, cmd := testUpdate(next, tea.MouseMsg{Type: tea.MouseRelease, Action: tea.MouseActionRelease, X: 30, Y: 22})
	if next.detailSel.active {
		tt.Fatal("release should clear the Info selection (text was copied)")
	}
	if cmd == nil {
		tt.Error("non-trivial Info drag should auto-copy via OSC 52")
	}

	// a plain click clears the Info selection
	next = testMouseUpdate(detailTestModel(), tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 20})
	next, cmd = testUpdate(next, tea.MouseMsg{Type: tea.MouseRelease, Action: tea.MouseActionRelease, X: 1, Y: 20})
	if next.detailSel.active {
		tt.Error("plain click must not finalize an Info selection")
	}
	if cmd != nil {
		tt.Error("plain Info click should not copy")
	}
}

func TestInfoSelectionReanchoredOnContentChange(tt *testing.T) {
	m := detailTestModel()
	m.containers = makeTestContainers(2)
	m.fitViewports()
	m.detailSel = textSel{active: true, anR: 1, anC: 2, endR: 4, endC: 5}
	prev := m.detailContent
	if prev == "" {
		tt.Fatal("detail content not built")
	}
	// a larger split gives the pane more rows, content unchanged -> keep
	m.height = 36
	m.fitViewports()
	if !m.detailSel.active {
		tt.Error("selection should survive an identical Info rebuild")
	}
	// switching to a container whose body no longer contains the selected
	// text must clear the selection instead of keeping stale coordinates
	m.selectedIdx = 1
	m.details = nil
	m.detailsID = ""
	m.fitViewports()
	if m.detailContent == prev {
		tt.Logf("note: switching containers did not change the body; re-anchor may legitimately keep")
	}
	if m.detailSel.active {
		tt.Error("selection should clear when the Info body no longer matches")
	}
}

func TestTextSelRowSpan(tt *testing.T) {
	s := textSel{anR: 0, anC: 1, endR: 2, endC: 3}
	if got, _, ok := s.rowSpan(0); !ok || got != 1 {
		tt.Errorf("row 0 from = %d ok=%v, want 1 true", got, ok)
	}
	if got, to, ok := s.rowSpan(1); !ok || got != 0 || to != -1 {
		tt.Errorf("row 1 span = (%d,%d) ok=%v, want (0,-1)", got, to, ok)
	}
	if got, to, ok := s.rowSpan(2); !ok || got != 0 || to != 3 {
		tt.Errorf("row 2 span = (%d,%d) ok=%v, want (0,3)", got, to, ok)
	}
	if _, _, ok := s.rowSpan(3); ok {
		tt.Error("row 3 should be outside the selection")
	}
	// up-right drag (anchor bottom, endpoint top)
	s = textSel{anR: 2, anC: 3, endR: 0, endC: 1}
	if got, _, ok := s.rowSpan(0); !ok || got != 1 {
		tt.Errorf("up-drag row 0 from = %d ok=%v, want 1", got, ok)
	}
	if got, to, ok := s.rowSpan(2); !ok || got != 0 || to != 3 {
		tt.Errorf("up-drag row 2 span = (%d,%d), want (0,3)", got, to)
	}
	// single-row drag in each direction
	s = textSel{anR: 1, anC: 5, endR: 1, endC: 2}
	if from, to, ok := s.rowSpan(1); !ok || from != 2 || to != 5 {
		tt.Errorf("leftward single-row span = (%d,%d) ok=%v, want (2,5)", from, to, ok)
	}
}

func TestTextSelIsTrivial(tt *testing.T) {
	if !(textSel{anR: 1, anC: 2, endR: 1, endC: 2}).isTrivial() {
		tt.Error("same-cell selection should be trivial (a click)")
	}
	if (textSel{anR: 1, anC: 2, endR: 1, endC: 3}).isTrivial() {
		tt.Error("two-cell selection should not be trivial")
	}
}

func withTrueColor(t *testing.T, f func()) {
	t.Helper()
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)
	f()
}

func TestDecorateSelectionSpansRows(tt *testing.T) {
	withTrueColor(tt, func() {
		content := "aaaa\nbbbb\ncccc"
		sel := textSel{active: true, anR: 0, anC: 1, endR: 2, endC: 2}
		out := decorateSelection(content, content, sel)

		if plain := stripANSI(out); plain != content {
			tt.Errorf("decorate changed visible text:\n got %q\nwant %q", plain, content)
		}
		lines := strings.Split(out, "\n")
		for i, want := range map[int]int{0: 1, 1: 1, 2: 1} {
			if n := strings.Count(lines[i], "48;2;44;73;46"); n != want {
				tt.Errorf("row %d has %d selection-highlight SGRs, want %d:\n%s", i, n, want, lines[i])
			}
		}
		// the first row keeps its unselected prefix
		first := stripANSI(lines[0])
		if !strings.HasPrefix(first, "a") || !strings.Contains(first, "aaaa") {
			tt.Errorf("row 0 should keep prefix then highlight: %q", first)
		}
	})
}

func TestDecorateSelectionPlainRowNoBareTail(tt *testing.T) {
	withTrueColor(tt, func() {
		// A selected plain row (padded to width w upstream) must never leave
		// unstyled cells right of the sel span: the span ends in a reset, so
		// the prefix AND suffix are re-painted with the theme background.
		m := New(nil)
		w := 80
		m.containers = makeTestContainers(1)
		sel := textSel{active: true, anR: 1, anC: 2, endR: 1, endC: 9}
		out := decorateSelection(m.buildDetailContent(w), "", sel)
		for i, ln := range strings.Split(out, "\n") {
			// the leak to guard against is a reset immediately followed by an
			// unstyled cell: those are default-bg spaces. A doubled reset has
			// no visible effect and shows up at styled-fill boundaries.
			if strings.Contains(ln, "\x1b[0m ") {
				tt.Errorf("row %d leaks unstyled cells after a reset: %q", i, ln)
			}
			if strings.Contains(ln, "Name") && !strings.Contains(ln, "48;2;44;73;46") {
				tt.Errorf("selected Name row missing highlight: %q", ln)
			}
		}
	})
}

func TestDecorateSelectionStyledRowPartialHighlight(tt *testing.T) {
	withTrueColor(tt, func() {
		// Selecting a slice of a styled row must keep the surrounding styling
		// (here cyan) and highlight only the exact span, like the Logs pane.
		styled := "\x1b[36mTITLE\x1b[0m"
		content := "plain\n" + styled
		sel := textSel{active: true, anR: 1, anC: 1, endR: 1, endC: 3}
		out := decorateSelection(content, "plain\nTITLE", sel)
		lines := strings.Split(out, "\n")
		if plain := stripANSI(lines[1]); plain != "TITLE" {
			tt.Errorf("styled row = %q, want stripped %q", plain, "TITLE")
		}
		if !strings.Contains(lines[1], "48;2;44;73;46") {
			tt.Errorf("styled row should carry the selection highlight, got: %q", lines[1])
		}
		// span is the middle cols 1..3 ("ITL"); the cyan prefix must survive
		// unchanged and the cyan-accent row must not be double-applied.
		if !strings.HasPrefix(lines[1], "\x1b[36mT") {
			tt.Errorf("unselected prefix lost its cyan styling: %q", lines[1])
		}
		if strings.Count(lines[1], "TITLE") != 0 {
			tt.Errorf("styled row must not double-apply text: %q", lines[1])
		}
		if !strings.HasSuffix(stripANSI(lines[1]), "E") || strings.Contains(lines[1], "\x1b[36mLE") {
			tt.Errorf("unselected tail must keep theme styling, not cyan: %q", lines[1])
		}
	})
}

func TestSelectedText(tt *testing.T) {
	content := "abc\ndef\nghij"
	sel := textSel{active: true, anR: 0, anC: 0, endR: 2, endC: 1}
	if got := selectedText(content, sel); got != "abc\ndef\ngh" {
		tt.Errorf("selectedText = %q, want %q", got, "abc\ndef\ngh")
	}
	// ansi row is stripped before slicing
	styled := "\x1b[32m" + "XY" + "\x1b[0m"
	sel = textSel{active: true, anR: 0, anC: 0, endR: 0, endC: 1}
	if got := selectedText(styled, sel); got != "XY" {
		tt.Errorf("styled selectedText = %q, want %q", got, "XY")
	}
	if got := selectedText(content, textSel{}); got != "" {
		tt.Errorf("inactive selection should yield empty text, got %q", got)
	}
}

func TestWrapLogCellsStripsAnsiAndCrLf(tt *testing.T) {
	in := "\x1b[31mred\x1b[0m one\r\n\x1b[32mgreen\x1b[0m two\r\n"
	got := wrapLogCells(in, 80)
	if want := "red one\ngreen two"; got != want {
		tt.Errorf("wrapLogCells = %q, want %q", got, want)
	}
	if strings.Contains(got, "\x1b") || strings.Contains(got, "\r") {
		tt.Errorf("wrapLogCells kept ANSI/CR: %q", got)
	}
}

func TestWrapLogCellsPlainASCIIIdentical(tt *testing.T) {
	// For plain single-cell text the cell-space wrap must behave exactly like
	// the previous rune-space wrap, so existing selection geometry holds.
	in := "aaa\naaattt\n\nttt"
	if got, want := wrapLogCells(in, 3), "aaa\naaa\nttt\n\nttt"; got != want {
		tt.Errorf("wrapLogCells = %q, want %q", got, want)
	}
}

func TestWrapLogCellsWideRunesNoSplit(tt *testing.T) {
	content := "中文測試abcdefgh"
	out := wrapLogCells(content, 6)
	for i, line := range strings.Split(out, "\n") {
		if cellw := runewidth.StringWidth(line); cellw > 6 {
			tt.Errorf("row %d exceeds 6 cells: %q (%d)", i, line, cellw)
		}
	}
	if joined := strings.Join(strings.Split(out, "\n"), ""); joined != content {
		tt.Errorf("wrap dropped runes: %q, want %q", joined, content)
	}
}

func TestCellToRuneColumnWideRow(tt *testing.T) {
	line := "ab中cd" // cells: a(1) b(1) 中(2) c(1) d(1) = 6
	for x, want := range map[int]int{0: 0, 1: 1, 2: 2, 3: 2, 4: 3, 5: 4, 6: 4, 10: 4} {
		col, ok := cellToRuneColumn(line, x)
		if !ok || col != want {
			tt.Errorf("cellToRuneColumn(%q, %d) = (%d,%v), want %d", line, x, col, ok, want)
		}
	}
}

func TestStyleLogRowsPaintsThemeEverywhere(tt *testing.T) {
	withTrueColor(tt, func() {
		sel := textSel{active: true, anR: 0, anC: 1, endR: 0, endC: 2}
		out := styleLogRows("aaa\nbbb", sel, 6, 4, 0)
		lines := strings.Split(out, "\n")
		if len(lines) != 4 {
			tt.Fatalf("styleLogRows rows = %d, want 4", len(lines))
		}
		for i, ln := range lines {
			if cellw := runewidth.StringWidth(stripANSI(ln)); cellw != 6 {
				tt.Errorf("row %d rendered width = %d, want 6: %q", i, cellw, ln)
			}
			if !strings.Contains(ln, "48;2;13;17;23") {
				tt.Errorf("row %d missing theme background: %q", i, ln)
			}
		}
		if trimmed := strings.TrimRight(stripANSI(lines[0]), " "); trimmed != "aaa" {
			tt.Errorf("row 0 text = %q, want %q", trimmed, "aaa")
		}
		if strings.Count(lines[0], "48;2;44;73;46") != 1 {
			tt.Errorf("row 0 should carry exactly one selection highlight: %q", lines[0])
		}
		if trimmed := strings.TrimRight(stripANSI(lines[1]), " "); trimmed != "bbb" {
			tt.Errorf("row 1 text = %q, want %q", trimmed, "bbb")
		}
	})
}

func TestLogDragSelectsAcrossRows(tt *testing.T) {
	m := logTestModel()
	// viewport row 0 == buffer row 1 (YOffset=1), content band starts at y=21
	press := tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 21}
	_ = press
	next := testMouseUpdate(m, press)
	if !next.dragSel {
		tt.Fatal("left press in the log body should start a drag")
	}
	if next.logSel.anR != 1 || next.logSel.anC != 0 {
		tt.Errorf("anchor = (%d,%d), want buffer (1,0)", next.logSel.anR, next.logSel.anC)
	}

	motion := tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, X: 4, Y: 23}
	next = testMouseUpdate(next, motion)
	if next.logSel.endR != 3 || next.logSel.endC != 3 {
		tt.Errorf("end = (%d,%d), want (3,3)", next.logSel.endR, next.logSel.endC)
	}

	release := tea.MouseMsg{Type: tea.MouseRelease, Action: tea.MouseActionRelease, X: 4, Y: 23}
	next, cmd := testUpdate(next, release)
	if next.dragSel {
		tt.Error("release should end the drag")
	}
	if next.logSel.active {
		tt.Error("release should clear the highlight (text was copied)")
	}
	if cmd == nil {
		tt.Error("non-trivial drag should auto-copy via OSC 52")
	}
}

func TestLogClickClearsSelection(tt *testing.T) {
	m := logTestModel()
	m.logSel = textSel{active: true, anR: 0, anC: 0, endR: 4, endC: 3}

	press := tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 22}
	next := testMouseUpdate(m, press)
	if !next.dragSel {
		tt.Fatal("press should start a fresh drag")
	}
	release := tea.MouseMsg{Type: tea.MouseRelease, Action: tea.MouseActionRelease, X: 1, Y: 22}
	next = testMouseUpdate(next, release)
	if next.dragSel {
		tt.Error("release should end the drag")
	}
	if next.logSel.active {
		tt.Error("a click (no drag) should clear the previous selection")
	}
}

func TestLogSelectionEscClears(tt *testing.T) {
	m := logTestModel()
	m.logSel = textSel{active: true, anR: 0, anC: 0, endR: 4, endC: 3}
	next := testMouseUpdate(m, tea.KeyMsg{Type: tea.KeyEsc})
	if next.logSel.active {
		tt.Error("esc should clear the selection")
	}
	if next.activeSubTab != subTabLogs {
		tt.Error("esc must not switch sub-tab while a selection is active")
	}
	// a second esc with no selection still falls back to switching to Info
	next2 := testMouseUpdate(next, tea.KeyMsg{Type: tea.KeyEsc})
	if next2.activeSubTab != subTabInfo {
		tt.Error("esc without selection should switch back to Info")
	}
}

func TestLogSelectionLeavingTabClearsKey(tt *testing.T) {
	m := logTestModel()
	m.logSel = textSel{active: true, anR: 0, anC: 0, endR: 4, endC: 3}
	next := testMouseUpdate(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("left")})
	if next.activeSubTab != subTabInfo {
		tt.Error("left arrow should switch to Info")
	}
	if next.logSel.active {
		tt.Error("leaving the Logs tab should clear the selection")
	}
}

func TestLogSelectionClearedWhenTextGoneOnFullReplace(tt *testing.T) {
	m := logTestModel()
	m.logSel = textSel{active: true, anR: 0, anC: 0, endR: 4, endC: 3}
	m.dragSel = true
	next := testMouseUpdate(m, containerLogMsg{id: m.containers[m.selectedIdx].ID, content: "fresh\nline"})
	if next.logSel.active || next.dragSel {
		tt.Error("full replace without the selected text should clear selection state")
	}
}

func TestLogSelectionKeptOnIdenticalFullReplace(tt *testing.T) {
	m := logTestModel()
	m.logSel = textSel{active: true, anR: 0, anC: 0, endR: 3, endC: 3}
	next := testMouseUpdate(m, containerLogMsg{id: m.containers[m.selectedIdx].ID, content: "alpha\nbeta\ngamma\ndelta\nepsilon\nzeta"})
	if !next.logSel.active {
		tt.Fatal("identical full reload must keep the selection active")
	}
	if next.logSel.anR != 0 || next.logSel.anC != 0 || next.logSel.endR != 3 || next.logSel.endC != 3 {
		tt.Errorf("selection drifted: %+v", next.logSel)
	}
}

func TestLogSelectionReanchoredAfterFullReplace(tt *testing.T) {
	m := logTestModel()
	// select "beta\ngamma\ndelt" (rows 1..3, cols 0..3)
	m.logSel = textSel{active: true, anR: 1, anC: 0, endR: 3, endC: 3}

	// a full reload that still contains the selected text (quiet container
	// re-issuing the same tail after a resync) must re-anchor, not clear
	content := "prelude\nalpha\nbeta\ngamma\ndelta\nepsilon\nzeta\ntail"
	next := testMouseUpdate(m, containerLogMsg{id: m.containers[m.selectedIdx].ID, content: content})
	if !next.logSel.active {
		tt.Fatal("full replace with matching text should keep the selection")
	}
	want := "beta\ngamma\ndelt"
	if got := selectedText(next.containerLogContent, next.logSel); got != want {
		tt.Errorf("re-anchored selectedText = %q, want %q", got, want)
	}
	// "beta" lands at row 2, "delt" ends at row 4 col 3 (inclusive)
	if next.logSel.anR != 2 || next.logSel.anC != 0 || next.logSel.endR != 4 || next.logSel.endC != 3 {
		tt.Errorf("re-anchored bounds = (%d,%d)-(%d,%d), want (2,0)-(4,3)",
			next.logSel.anR, next.logSel.anC, next.logSel.endR, next.logSel.endC)
	}
	// decoration renders the highlight on the shifted rows
	dec := decorateSelection(next.containerLogContent, "", next.logSel)
	for _, wantLine := range []string{"beta", "gamma", "delt"} {
		found := strings.Contains(dec, selTextStyle.Render(wantLine))
		if !found {
			tt.Errorf("decorated content missing highlighted %q", wantLine)
		}
	}
}

func TestLogSlowDragSurvivesIdenticalReload(tt *testing.T) {
	// A quiet container gets a byte-identical full reload on every tick once
	// its cursor goes stale. A slow drag that has a tick land mid-drag must
	// not be destroyed by that reload.
	m := logTestModel()
	press := tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 21}
	next := testMouseUpdate(m, press)
	if !next.dragSel {
		tt.Fatal("press should start a drag")
	}
	next = testMouseUpdate(next, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, X: 4, Y: 22})
	if next.logSel.endR != 2 {
		tt.Fatalf("motion end row = %d, want 2", next.logSel.endR)
	}

	// identical full reload lands in the middle of the drag
	reload := containerLogMsg{id: next.containers[next.selectedIdx].ID, content: "alpha\nbeta\ngamma\ndelta\nepsilon\nzeta"}
	next = testMouseUpdate(next, reload)
	if !next.dragSel || next.logSel.active {
		tt.Fatalf("identical reload destroyed the in-flight drag: dragSel=%v sel=%+v", next.dragSel, next.logSel)
	}
	if next.logSel.anR != 1 || next.logSel.anC != 0 || next.logSel.endR != 2 || next.logSel.endC != 3 {
		tt.Errorf("drag bounds after identical reload = (%d,%d)-(%d,%d), want (1,0)-(2,3)",
			next.logSel.anR, next.logSel.anC, next.logSel.endR, next.logSel.endC)
	}

	// continued motion after the tick must still extend the drag
	next = testMouseUpdate(next, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, X: 4, Y: 23})
	if next.logSel.endR != 3 {
		tt.Errorf("motion after tick end row = %d, want 3", next.logSel.endR)
	}
	next, cmd := testUpdate(next, tea.MouseMsg{Type: tea.MouseRelease, Action: tea.MouseActionRelease, X: 4, Y: 23})
	if next.logSel.active {
		tt.Error("release should clear the highlight (text was copied)")
	}
	if cmd == nil {
		tt.Error("surviving drag release should auto-copy")
	}
}

func TestLogInFlightDragDroppedOnChangedReload(tt *testing.T) {
	// A genuinely different full reload (container recreated, log rotated)
	// mid-drag cancels the drag: the old rows no longer exist.
	m := logTestModel()
	press := tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 21}
	next := testMouseUpdate(m, press)
	if !next.dragSel {
		tt.Fatal("press should start a drag")
	}
	next = testMouseUpdate(next, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, X: 4, Y: 23})
	next = testMouseUpdate(next, containerLogMsg{id: next.containers[next.selectedIdx].ID, content: "totally\ndifferent\nlog"})
	if next.dragSel || next.logSel.active {
		tt.Error("changed full reload should cancel the in-flight drag")
	}
}

func testUpdate(m Model, msg tea.Msg) (Model, tea.Cmd) {
	next, cmd := m.Update(msg)
	return next.(Model), cmd
}

func TestOsc52Sequence(tt *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte("beta\nx"))
	if want := "\x1b]52;c;" + b64 + "\x1b\\"; osc52Sequence("beta\nx") != want {
		tt.Errorf("sequence = %q, want %q", osc52Sequence("beta\nx"), want)
	}
	rus := "выделение"
	if got := osc52Sequence(rus); got != "\x1b]52;c;"+base64.StdEncoding.EncodeToString([]byte(rus))+"\x1b\\" {
		tt.Error("non-ASCII text should be UTF-8 base64 encoded")
	}
}

func TestLogReleaseAutoCopiesSelection(tt *testing.T) {
	m := logTestModel()
	next := testMouseUpdate(m, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 21})
	next = testMouseUpdate(next, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, X: 4, Y: 23})
	next, cmd := testUpdate(next, tea.MouseMsg{Type: tea.MouseRelease, Action: tea.MouseActionRelease, X: 4, Y: 23})
	if next.logSel.active {
		tt.Fatal("release of a real drag should clear the selection (text was copied)")
	}
	if cmd == nil {
		tt.Error("non-trivial release should return an OSC 52 copy command")
	}
	// a plain click (release on the same cell) must not auto-copy
	next = testMouseUpdate(logTestModel(), tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 21})
	next, cmd = testUpdate(next, tea.MouseMsg{Type: tea.MouseRelease, Action: tea.MouseActionRelease, X: 1, Y: 21})
	if next.logSel.active {
		tt.Fatal("plain click must not finalize an active selection")
	}
	if cmd != nil {
		tt.Error("plain click should not copy to the clipboard")
	}
}

func TestCopyKeyCopiesFinalizedSelection(tt *testing.T) {
	m := logTestModel()
	next := testMouseUpdate(m, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 21})
	next = testMouseUpdate(next, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, X: 4, Y: 22})
	// a lost release (timeout) keeps the selection active so y can copy it
	next, _ = testUpdate(next, dragTimeoutMsg{gen: next.dragGen})
	if !next.logSel.active {
		tt.Fatal("timed-out drag should keep an active selection for a manual copy")
	}
	keyY := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}
	if _, cmd := testUpdate(next, keyY); cmd == nil {
		tt.Error("y with an active selection should return a copy command")
	}
	next.logSel = textSel{}
	if _, cmd := testUpdate(next, keyY); cmd != nil {
		tt.Error("y without a selection should return no command")
	}
	// a completed drag resets immediately: y right after must be a no-op
	next2 := testMouseUpdate(m, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 21})
	next2 = testMouseUpdate(next2, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, X: 4, Y: 22})
	next2 = testMouseUpdate(next2, tea.MouseMsg{Type: tea.MouseRelease, Action: tea.MouseActionRelease, X: 4, Y: 22})
	if next2.logSel.active {
		tt.Fatal("completed drag should have cleared the selection")
	}
	if _, cmd := testUpdate(next2, keyY); cmd != nil {
		tt.Error("y after a completed drag must not copy (selection reset)")
	}
}

func TestLogSelectionReanchorCellBoundaries(tt *testing.T) {
	sels, ok := reanchorSelection("a\nbcde", textSel{active: true, anR: 1, anC: 1, endR: 1, endC: 3}, "x\na\nbcde")
	if !ok {
		tt.Fatal("expected re-anchoring to succeed")
	}
	if sels.anR != 2 || sels.anC != 1 || sels.endR != 2 || sels.endC != 3 {
		tt.Errorf("mid-line re-anchor = (%d,%d)-(%d,%d), want (2,1)-(2,3)", sels.anR, sels.anC, sels.endR, sels.endC)
	}
	// full-line selection "bcde" (cols 0..3 inclusive), shifted down two rows
	sels, ok = reanchorSelection("a\nbcde", textSel{active: true, anR: 1, anC: 0, endR: 1, endC: 3}, "y\na\nbcde")
	if !ok {
		tt.Fatal("expected full-line re-anchoring to succeed")
	}
	if sels.anR != 2 || sels.anC != 0 || sels.endR != 2 || sels.endC != 3 {
		tt.Errorf("full-line re-anchor = (%d,%d)-(%d,%d), want (2,0)-(2,3)", sels.anR, sels.anC, sels.endR, sels.endC)
	}
	// a single-cell selection that matches the first row of the new buffer
	sels, ok = reanchorSelection("xy", textSel{active: true, anR: 0, anC: 0, endR: 0, endC: 0}, "x\nxyz")
	if !ok {
		tt.Fatal("expected cross-boundary re-anchoring to succeed")
	}
	if got := selectedText("x\nxyz", sels); got != "x" {
		tt.Errorf("cross-boundary selectedText = %q, want %q", got, "x")
	}
}

func TestLogSelectionClearedOnPrune(tt *testing.T) {
	m := logTestModel()
	filler := strings.Repeat("x\n", 2*logMaxWrappedLines+1)
	m.containerLogContent = filler
	m.logSel = textSel{active: true, anR: 0, anC: 0, endR: 4, endC: 3}
	msg := containerLogMsg{id: m.containers[m.selectedIdx].ID, content: strings.Repeat("y\n", 200), incremental: true}
	next := testMouseUpdate(m, msg)
	if next.logSel.active {
		tt.Error("pruning the log buffer should clear buffer-anchored selection")
	}
}

func TestWheelStillScrollsLogsWithSelection(tt *testing.T) {
	m := logTestModel()
	m.logSel = textSel{active: true, anR: 0, anC: 0, endR: 4, endC: 3}
	m.containerLogViewport.SetContent(strings.Repeat("filler line\n", 30))
	m.containerLogViewport.YOffset = 1

	wheel := tea.MouseMsg{Type: tea.MouseWheelDown, Action: tea.MouseActionPress,
		Button: tea.MouseButtonWheelDown, X: 20, Y: 25}
	next := testMouseUpdate(m, wheel)
	if next.containerLogViewport.YOffset <= 1 {
		tt.Error("wheel down should scroll the log viewport even with a selection active")
	}
	if !next.logSel.active {
		tt.Error("wheel scroll must not clear the selection")
	}
}

func TestLogSelectionSurvivesScroll(tt *testing.T) {
	m := logTestModel()
	press := tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 21}
	motion := tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, X: 4, Y: 23}
	m = testMouseUpdate(testMouseUpdate(m, press), motion)
	// a lost release (timeout) leaves the buffer-anchored selection active
	next, _ := testUpdate(m, dragTimeoutMsg{gen: m.dragGen})
	if want := "beta\ngamma\ndelt"; selectedText(next.containerLogContent, next.logSel) != want {
		tt.Fatalf("pre-scroll selectedText = %q, want %q", selectedText(next.containerLogContent, next.logSel), want)
	}

	next.containerLogViewport.SetContent(strings.Repeat("filler\n", 30))
	next.containerLogViewport.YOffset = 10
	// selection rows are buffer-anchored, not screen-anchored, so the
	// selected text is unchanged even though the view moved
	if got := selectedText(next.containerLogContent, next.logSel); got != "beta\ngamma\ndelt" {
		tt.Errorf("post-scroll selectedText = %q, want %q (selection drifted)", got, "beta\ngamma\ndelt")
	}
}

// TestViewHeightMatchesTerminal guards against any component that wraps or
// overflows: if View() is taller than the terminal, the visible content
// scrolls by one row and mouse→buffer coordinates silently shift.
func TestViewHeightMatchesTerminal(tt *testing.T) {
	withTrueColor(tt, func() {
		for _, cfg := range [][2]int{{24, 130}, {30, 130}, {30, 140}, {36, 160}, {20, 80}, {40, 100}} {
			h, w := cfg[0], cfg[1]
			m := detailTestModel()
			m.height = h
			m.width = w
			m.fitViewports()
			if n := len(strings.Split(m.View(), "\n")); n != h {
				tt.Errorf("h=%d w=%d: View() rows=%d, want %d", h, w, n, h)
			}
			if n := len(strings.Split(stripANSI(m.renderHelpBar()), "\n")); n != 1 {
				tt.Errorf("h=%d w=%d: help bar rows=%d, want 1", h, w, n)
			}
		}
	})
}

// TestLogDragTimeoutFinalizes does the manual release never arrive
// (lost focus / drag ended outside the terminal), a stale in-flight drag
// must be snapped into a final selection WITHOUT auto-copying.
func TestLogDragTimeoutFinalizes(tt *testing.T) {
	m := logTestModel()
	press := tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 21}
	next := testMouseUpdate(m, press)
	if !next.dragSel {
		tt.Fatal("press should start a drag")
	}
	next = testMouseUpdate(next, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, X: 4, Y: 23})

	next, cmd := testUpdate(next, dragTimeoutMsg{gen: next.dragGen})
	if next.dragSel {
		tt.Error("timeout should end the in-flight drag")
	}
	if !next.logSel.active {
		tt.Error("timeout should finalize the selection")
	}
	if cmd != nil {
		tt.Error("timed-out drag must not auto-copy")
	}
	if want := "beta\ngamma\ndelt"; selectedText(next.containerLogContent, next.logSel) != want {
		tt.Errorf("finalized text = %q, want %q", selectedText(next.containerLogContent, next.logSel), want)
	}
}

// TestDragTimeoutStaleGenIgnored proves a timer from an outlived drag
// generation cannot kill a fresher drag.
func TestDragTimeoutStaleGenIgnored(tt *testing.T) {
	m := logTestModel()
	press := tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 21}
	next := testMouseUpdate(m, press)
	gen0 := next.dragGen
	next = testMouseUpdate(next, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, X: 4, Y: 23})

	if _, cmd := testUpdate(next, dragTimeoutMsg{gen: gen0}); cmd != nil {
		tt.Error("stale timeout returned a cmd")
	}
	if !next.dragSel {
		tt.Fatal("stale timeout must not touch a newer drag")
	}

	// the correctly-generated timer still finalizes it
	next, cmd := testUpdate(next, dragTimeoutMsg{gen: next.dragGen})
	if next.dragSel || !next.logSel.active {
		tt.Error("matching timeout should finalize the drag")
	}
	if cmd != nil {
		tt.Error("matching timeout must not auto-copy")
	}

	// a plain release after finalization still ends any new drag normally
	next = testMouseUpdate(detailTestModel(), tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 20})
	next, cmd = testUpdate(next, tea.MouseMsg{Type: tea.MouseRelease, Action: tea.MouseActionRelease, X: 1, Y: 20})
	if cmd != nil || next.detailDragSel {
		tt.Error("plain Info click must not copy or leave a drag")
	}
}

// TestInfoDragTimeoutFinalizes mirrors the Logs timeout for the Info pane.
func TestInfoDragTimeoutFinalizes(tt *testing.T) {
	m := detailTestModel()
	press := tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 20}
	next := testMouseUpdate(m, press)
	if !next.detailDragSel {
		tt.Fatal("press in the Info body should start a drag")
	}
	next = testMouseUpdate(next, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, X: 30, Y: 22})

	next, cmd := testUpdate(next, dragTimeoutMsg{gen: next.dragGen})
	if next.detailDragSel {
		tt.Error("timeout should end the in-flight Info drag")
	}
	if !next.detailSel.active {
		tt.Error("timeout should finalize the Info selection")
	}
	if cmd != nil {
		tt.Error("timed-out Info drag must not auto-copy")
	}
	if selectedText(next.detailContent, next.detailSel) == "" {
		tt.Error("finalized Info selection covers no text")
	}
}

func TestCopyToastArmedOnRelease(tt *testing.T) {
	m := detailTestModel()
	next := testMouseUpdate(m, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 20})
	next = testMouseUpdate(next, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, X: 30, Y: 22})
	released, cmd := testUpdate(next, tea.MouseMsg{Type: tea.MouseRelease, Action: tea.MouseActionRelease, X: 30, Y: 22})
	if cmd == nil {
		tt.Error("drag release should return the copy command")
	}
	if !released.copyToast {
		tt.Error("release that copies should arm the copied-toast badge")
	}
	if released.copyToastGen == 0 {
		tt.Error("copied-toast gen should bump on a copy")
	}
	// a stale timer from an older copy must not hide a freshly armed toast
	if again, _ := testUpdate(released, copyToastMsg{gen: released.copyToastGen - 1}); !again.copyToast {
		tt.Error("stale copyToastMsg must not clear the current toast")
	}
	// the matching expiry timer hides it
	if expired, _ := testUpdate(released, copyToastMsg{gen: released.copyToastGen}); expired.copyToast {
		tt.Error("matching copyToastMsg should clear the toast")
	}
}

func TestCopyToastViaCopyKey(tt *testing.T) {
	m := logTestModel()
	next := testMouseUpdate(m, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 21})
	next = testMouseUpdate(next, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, X: 4, Y: 22})
	// lost release (timeout) keeps the selection active for a manual copy
	next, _ = testUpdate(next, dragTimeoutMsg{gen: next.dragGen})
	if !next.logSel.active {
		tt.Fatal("timed-out drag should keep an active selection")
	}
	keyY := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}
	copied, cmd := testUpdate(next, keyY)
	if cmd == nil {
		tt.Fatal("y with an active selection should copy")
	}
	if !copied.copyToast {
		tt.Error("copy key should arm the copied-toast badge")
	}
	// y without a selection neither copies nor arms the toast
	if noop, noCmd := testUpdate(logTestModel(), keyY); noCmd != nil || noop.copyToast {
		tt.Error("y without a selection must neither copy nor arm the toast")
	}
}

func TestCopyToastNotOnPlainClick(tt *testing.T) {
	m := detailTestModel()
	next := testMouseUpdate(m, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 20})
	cl, cmd := testUpdate(next, tea.MouseMsg{Type: tea.MouseRelease, Action: tea.MouseActionRelease, X: 1, Y: 20})
	if cmd != nil {
		tt.Error("plain click should not copy")
	}
	if cl.copyToast {
		tt.Error("plain click must not arm the copied-toast badge")
	}
}

func TestRenderContainerMenuShape(tt *testing.T) {
	// lipgloss downgrades to the Ascii profile when stdout is not a TTY;
	// force TrueColor so the emitted SGR sequences assert the real palette.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	tt.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	m := detailTestModel()
	m.containers = makeTestContainers(3)
	m.menu = m.buildContainerMenu()
	tt.Logf("popup at (%d,%d) %dx%d:\n%s", m.menu.x, m.menu.y, m.menu.w, m.menu.h,
		strings.Join(m.renderContainerMenu(), "\n"))

	rows := m.renderContainerMenu()
	if len(rows) != m.menu.h {
		tt.Errorf("menu rows = %d, want %d", len(rows), m.menu.h)
	}
	for _, r := range rows {
		if lipgloss.Width(r) != m.menu.w {
			tt.Errorf("menu row width = %d, want %d: %q", lipgloss.Width(r), m.menu.w, stripANSI(r))
		}
	}
	wantTitle := "Действия с контейнером " + containerDisplayName(m.containers[0])
	if got := strings.TrimSpace(strings.Trim(stripANSI(rows[1]), "│")); got != wantTitle {
		tt.Errorf("title = %q, want %q", got, wantTitle)
	}
	if got := strings.TrimSpace(strings.Trim(stripANSI(rows[2]), "│")); got != "Stop" {
		tt.Errorf("first item = %q, want a Stop row for a running container", got)
	}
	if got := strings.TrimSpace(strings.Trim(stripANSI(rows[3]), "│")); got != "Remove" {
		tt.Errorf("second item = %q, want Remove", got)
	}
	if got := strings.TrimSpace(strings.Trim(stripANSI(rows[4]), "│")); got != "Remove with data" {
		tt.Errorf("third item = %q, want Remove with data", got)
	}

	// the selected row (sel=0) is inverted with the accent background; the
	// other rows are not
	activeBG := ";48;2;46;160;67m"
	if !strings.Contains(rows[2], activeBG) {
		tt.Error("selected menu row must carry the accent background")
	}
	if strings.Contains(rows[3], activeBG) {
		tt.Error("unselected menu row must not carry the accent background")
	}
	if strings.Contains(rows[4], activeBG) {
		tt.Error("unselected menu row must not carry the accent background")
	}

	// the box is centered within the terminal, clear of the tab bar
	wantX := max((m.width-m.menu.w)/2, 0)
	if m.menu.x != wantX {
		tt.Errorf("menu.x = %d, want centered %d", m.menu.x, wantX)
	}
	wantY := max((m.height-m.menu.h)/2, tabBarHeight+1)
	if m.menu.y != wantY {
		tt.Errorf("menu.y = %d, want centered %d", m.menu.y, wantY)
	}
	if m.menu.y <= tabBarHeight {
		tt.Errorf("menu must clear the tab bar, y = %d", m.menu.y)
	}

	// two-stage Remove: entering the confirm swap rebuilds the box with a
	// header naming the target plus Yes/No items
	m.enterRemoveConfirm(false)
	crows := m.renderContainerMenu()
	if len(crows) != m.menu.h {
		tt.Errorf("confirm rows = %d, want %d", len(crows), m.menu.h)
	}
	wantHeader := "Remove " + containerDisplayName(m.containers[m.selectedIdx]) + "?"
	if got := strings.TrimSpace(strings.Trim(stripANSI(crows[1]), "│")); got != wantHeader {
		tt.Errorf("confirm header = %q, want %q", got, wantHeader)
	}
	if got := strings.TrimSpace(strings.Trim(stripANSI(crows[2]), "│")); got != "Yes, remove" {
		tt.Errorf("confirm yes = %q", got)
	}
	if !strings.Contains(crows[2], activeBG) {
		tt.Error("confirm resets the cursor to the first (Yes, remove) item")
	}
	if got := strings.TrimSpace(strings.Trim(stripANSI(crows[3]), "│")); got != "No, cancel" {
		tt.Errorf("confirm no = %q", got)
	}

	// the "Remove with data" variant: same confirm layout, its own header and
	// the Yes item still selected
	m.enterRemoveConfirm(true)
	drows := m.renderContainerMenu()
	if len(drows) != m.menu.h {
		tt.Errorf("data-confirm rows = %d, want %d", len(drows), m.menu.h)
	}
	wantDataHeader := "Remove " + containerDisplayName(m.containers[m.selectedIdx]) + " and its volumes?"
	if got := strings.TrimSpace(strings.Trim(stripANSI(drows[1]), "│")); got != wantDataHeader {
		tt.Errorf("data-confirm header = %q, want %q", got, wantDataHeader)
	}
	if !strings.Contains(drows[2], activeBG) {
		tt.Error("data-confirm resets the cursor to the first (Yes, remove) item")
	}
}

func TestRightClickDoesNotOpenMenu(tt *testing.T) {
	m := detailTestModel()
	m.containers = makeTestContainers(3)
	m.fitViewports()

	// ПКМ больше не открывает контекстное меню — только клавиша x на
	// выбранной строке, и поэтому она не двигает выделение.
	for _, y := range []int{5, 6, 12} {
		next := testMouseUpdate(m, tea.MouseMsg{Type: tea.MouseRight, Action: tea.MouseActionPress, X: 20, Y: y})
		if next.menuOpen {
			tt.Fatalf("right-click at y=%d opened the popup", y)
		}
		if next.selectedIdx != 0 {
			tt.Errorf("right-click at y=%d moved the selection to %d", y, next.selectedIdx)
		}
	}
	// release likewise
	next := testMouseUpdate(m, tea.MouseMsg{Type: tea.MouseRight, Action: tea.MouseActionRelease, X: 20, Y: 5})
	if next.menuOpen {
		tt.Fatal("right release opened the popup")
	}
}

func TestMenuNavigationAndEsc(tt *testing.T) {
	m := detailTestModel()
	m.containers = makeTestContainers(3)
	m.fitViewports()
	m = openMenuFor(m, 0)

	down := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")}
	up := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")}

	next := testMouseUpdate(m, down)
	if next.menu.sel != 1 {
		tt.Errorf("down = %d, want 1", next.menu.sel)
	}
	next = testMouseUpdate(next, down)
	if next.menu.sel != 2 {
		tt.Errorf("down = %d, want 2", next.menu.sel)
	}
	next = testMouseUpdate(next, down)
	if next.menu.sel != 2 {
		tt.Errorf("down past the last item = %d, want 2 (clamped)", next.menu.sel)
	}
	next = testMouseUpdate(next, up)
	if next.menu.sel != 1 {
		tt.Errorf("up = %d, want 1", next.menu.sel)
	}
	next = testMouseUpdate(next, up)
	if next.menu.sel != 0 {
		tt.Errorf("up = %d, want 0", next.menu.sel)
	}
	next = testMouseUpdate(next, up)
	if next.menu.sel != 0 {
		tt.Errorf("up past the first item = %d, want 0 (clamped)", next.menu.sel)
	}

	next, cmd := testUpdate(next, tea.KeyMsg{Type: tea.KeyEsc})
	if next.menuOpen {
		tt.Fatal("Esc should close the popup")
	}
	if cmd != nil {
		tt.Error("Esc must not dispatch an action")
	}

	// a stray key closes the popup and is swallowed: it must not leak into
	// the normal key handling (restart would run, selection would move)
	m = openMenuFor(detailTestModel(), 0)
	next, _ = testUpdate(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if next.menuOpen {
		tt.Fatal("a stray key should close the popup")
	}
	if next.selectedIdx != 0 {
		tt.Errorf("stray key leaked into normal handling, selection = %d", next.selectedIdx)
	}
}

func TestMenuActivateStartStop(tt *testing.T) {
	m := detailTestModel()
	m.containers = makeTestContainers(3)
	m.fitViewports()
	m = openMenuFor(m, 1) // exited
	if m.selectedIdx != 1 {
		tt.Fatalf("selected row = %d, want 1", m.selectedIdx)
	}
	if got := m.menu.items[0].label; got != "Start" {
		tt.Errorf("first item = %q, want Start for an exited container", got)
	}
	// Enter on the action closes the popup and dispatches toggleContainer
	next, cmd := testUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	if next.menuOpen {
		tt.Fatal("Enter on an action should close the popup")
	}
	if cmd == nil {
		tt.Error("Enter on Start/Stop should dispatch a toggle command")
	}
}

func TestMenuClickOutsideCloses(tt *testing.T) {
	m := detailTestModel()
	m.containers = makeTestContainers(3)
	m.fitViewports()
	m = openMenuFor(m, 0)

	// left-click on the Remove item (title row + one item above it, plus the
	// top border) activates it and keeps the popup open in the confirm stage
	itemY := m.menu.y + 3 + 1 // 0-based row -> 1-based mouse Y
	itemX := m.menu.x + 3 + 1
	next := testMouseUpdate(m, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: itemX, Y: itemY})
	if !next.menuOpen {
		tt.Fatal("click on the Remove item should keep the popup open (confirm stage)")
	}
	if !next.menu.confirm {
		tt.Fatal("Remove activation should enter the confirm stage")
	}

	// left-click clearly outside the box (right and below) closes the popup;
	// the confirm stage widened the box, so measure against next's geometry
	outX := next.menu.x + next.menu.w + 1
	next = testMouseUpdate(next, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: outX, Y: next.menu.y + next.menu.h + 1})
	if next.menuOpen {
		tt.Fatal("click outside the popup should close it")
	}

	// motion must not close; a wheel press closes. Open a fresh popup for this
	// (m is still open from the first right-click, and a right-press while a
	// popup is open deliberately closes it rather than re-anchoring).
	m = openMenuFor(detailTestModel(), 0)
	m = testMouseUpdate(m, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, X: 20, Y: 5})
	if !m.menuOpen {
		tt.Fatal("mouse motion must not close the popup")
	}
	m = testMouseUpdate(m, tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress, X: 20, Y: 5})
	if m.menuOpen {
		tt.Fatal("wheel should close the popup")
	}
}

func TestMenuRemoveFlows(tt *testing.T) {
	down := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")}
	enter := tea.KeyMsg{Type: tea.KeyEnter}

	openAt := func(idx int) Model {
		return openMenuFor(detailTestModel(), idx)
	}

	// plain Remove: down to it, Enter stages the confirm, Enter confirms Yes
	m := openAt(0) // row 0
	next, cmd := testUpdate(m, down)
	if next.menu.sel != 1 {
		tt.Fatalf("down = %d, want Remove at 1", next.menu.sel)
	}
	next, cmd = testUpdate(next, enter)
	if !next.menu.confirm {
		tt.Fatal("Enter on Remove should stage the confirm")
	}
	if got := next.menu.header; got != "Remove test-container-0?" {
		tt.Errorf("confirm header = %q", got)
	}
	if got := next.menu.items[0].label; got != "Yes, remove" {
		tt.Errorf("confirm yes = %q", got)
	}
	next, cmd = testUpdate(next, enter)
	if next.menuOpen {
		tt.Fatal("Yes on the confirm should close the popup")
	}
	if cmd == nil {
		tt.Error("Yes must dispatch the remove command")
	}

	// Escape in the confirm stage cancels without dispatching
	m = openAt(0)
	next, _ = testUpdate(m, down)
	next, _ = testUpdate(next, enter)
	next, cmd = testUpdate(next, tea.KeyMsg{Type: tea.KeyEsc})
	if next.menuOpen {
		tt.Fatal("Esc in the confirm should close the popup")
	}
	if cmd != nil {
		tt.Error("Esc must not dispatch a command")
	}

	// "No, cancel" closes the popup without dispatching
	m = openAt(0)
	next, _ = testUpdate(m, down)
	next, _ = testUpdate(next, enter)
	next, _ = testUpdate(next, down) // sel moves to "No, cancel"
	if got := next.menu.items[next.menu.sel].label; got != "No, cancel" {
		tt.Fatalf("down in confirm = %d (%q)", next.menu.sel, got)
	}
	next, cmd = testUpdate(next, enter)
	if next.menuOpen {
		tt.Fatal("No, cancel should close the popup")
	}
	if cmd != nil {
		tt.Error("No, cancel must not dispatch a command")
	}

	// Remove with data: down twice, Enter, confirm header mentions volumes
	m = openAt(0)
	next, _ = testUpdate(m, down)
	next, _ = testUpdate(next, down)
	if next.menu.sel != 2 {
		tt.Fatalf("down down = %d, want Remove with data at 2", next.menu.sel)
	}
	next, _ = testUpdate(next, enter)
	if !next.menu.confirm {
		tt.Fatal("Enter on Remove with data should stage the confirm")
	}
	if got := next.menu.header; got != "Remove test-container-0 and its volumes?" {
		tt.Errorf("data confirm header = %q", got)
	}
	next, cmd = testUpdate(next, enter)
	if next.menuOpen {
		tt.Fatal("Yes on the data confirm should close the popup")
	}
	if cmd == nil {
		tt.Error("Yes must dispatch the remove-with-data command")
	}
}

func TestKeyXOpensContextMenu(tt *testing.T) {
	m := detailTestModel()
	m.containers = makeTestContainers(3)
	m.fitViewports()
	m.selectedIdx = 2

	x := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}

	next, cmd := testUpdate(m, x)
	if !next.menuOpen {
		tt.Fatal("x should open the popup on the selected row")
	}
	if next.selectedIdx != 2 {
		tt.Errorf("x changed the selection to %d", next.selectedIdx)
	}
	if cmd != nil {
		tt.Error("x must not dispatch an action")
	}
	if got := next.menu.items[0].label; got != "Start" {
		tt.Errorf("first item = %q, want Start for the restarting container", got)
	}
	if got := next.menu.items[2].label; got != "Remove with data" {
		tt.Errorf("third item = %q", got)
	}
	if got := next.menu.header; got != "Действия с контейнером test-container-2" {
		tt.Errorf("menu title = %q", got)
	}
	// the popup is centered on the screen, clear of the tab bar
	if want := max((next.width-next.menu.w)/2, 0); next.menu.x != want {
		tt.Errorf("popup x = %d, want centered %d", next.menu.x, want)
	}
	if want := max((next.height-next.menu.h)/2, tabBarHeight+1); next.menu.y != want {
		tt.Errorf("popup y = %d, want centered %d", next.menu.y, want)
	}

	// x while the popup is open closes it (stray key), without dispatching
	next, _ = testUpdate(next, x)
	if next.menuOpen {
		tt.Fatal("x while a popup is open should close it")
	}

	// j/k move the selection; x then opens the centered popup for the new row
	next, _ = testUpdate(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if next.selectedIdx != 1 {
		tt.Fatalf("k = %d, want 1", next.selectedIdx)
	}
	next, _ = testUpdate(next, x)
	if !next.menuOpen {
		tt.Fatal("x after k should open the popup")
	}
	if got := next.menu.header; got != "Действия с контейнером test-container-1" {
		tt.Errorf("menu title after k = %q", got)
	}
	if want := max((next.width-next.menu.w)/2, 0); next.menu.x != want {
		tt.Errorf("popup x after k = %d, want centered %d", next.menu.x, want)
	}

	// x on another tab must not open the popup
	m.activeTab = tabImages
	next, _ = testUpdate(m, x)
	if next.menuOpen {
		tt.Fatal("x must not open the popup outside the Containers tab")
	}
}

func TestTabBarToastBadge(tt *testing.T) {
	m := detailTestModel()
	m.copyToast = false
	off := m.renderTabBar()
	if strings.Contains(off, "Copied to clipboard") {
		tt.Error("badge must be hidden when no copy happened")
	}
	if rows := strings.Count(off, "\n"); rows != 2 {
		tt.Errorf("tab bar should stay 3 rows without the badge, got %d", rows+1)
	}

	m.copyToast = true
	on := m.renderTabBar()
	if !strings.Contains(on, "Copied to clipboard") {
		tt.Error("badge should appear when a copy happened")
	}
	if rows := strings.Count(on, "\n"); rows != 2 {
		tt.Errorf("badge must not change the tab bar height, got %d rows", rows+1)
	}

	// narrow terminal: the badge is skipped rather than wrapping the tab row
	narrow := m
	narrow.width = 72
	n := narrow.renderTabBar()
	if strings.Contains(n, "Copied to clipboard") {
		tt.Error("badge should be skipped when the terminal is too narrow")
	}
}
