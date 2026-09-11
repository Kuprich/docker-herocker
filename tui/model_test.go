package tui

import (
	"encoding/base64"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/creack/pty"
	xterm "github.com/gitpod-io/xterm-go"
	"github.com/kuri4/dockerherocker/docker"
	"github.com/mattn/go-runewidth"
	"github.com/muesli/termenv"
	"golang.org/x/sys/unix"
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

func TestClassifyImage(tt *testing.T) {
	cases := []struct {
		name       string
		containers int64
		ntags      int
		wantLabel  string
		wantDot    string
	}{
		{"in-use by a container", 2, 1, "IN-USE", "●"},
		{"tagged but unused", 0, 1, "UNUSED", "●"},
		{"unknown container count treated as unused", -1, 1, "UNUSED", "●"},
		{"dangling (no tags)", 0, 0, "DANGLING", "○"},
		{"in-use even when tagless", 1, 0, "IN-USE", "●"},
	}
	for _, c := range cases {
		st := classifyImage(c.containers, c.ntags)
		if st.label != c.wantLabel {
			tt.Errorf("%s: label = %q, want %q", c.name, st.label, c.wantLabel)
		}
		if st.dot != c.wantDot {
			tt.Errorf("%s: dot = %q, want %q", c.name, st.dot, c.wantDot)
		}
	}
}

func TestClassifyUsage(tt *testing.T) {
	cases := []struct {
		name      string
		refCount  int64
		wantLabel string
		wantDot   string
	}{
		{"referenced by a container", 1, "IN-USE", "●"},
		{"referenced by several containers", 3, "IN-USE", "●"},
		{"unused", 0, "UNUSED", "○"},
		{"unknown ref count treated as unused", -1, "UNUSED", "○"},
	}
	for _, c := range cases {
		st := classifyUsage(c.refCount)
		if st.label != c.wantLabel {
			tt.Errorf("%s: label = %q, want %q", c.name, st.label, c.wantLabel)
		}
		if st.dot != c.wantDot {
			tt.Errorf("%s: dot = %q, want %q", c.name, st.dot, c.wantDot)
		}
	}
	if got := classifyUsage(1).color; got != t.Success {
		tt.Errorf("IN-USE color = %q, want success green", got)
	}
	if got := classifyUsage(0).color; got != t.Muted {
		tt.Errorf("UNUSED color = %q, want muted gray", got)
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

func TestFormatCPU(tt *testing.T) {
	cases := map[float64]string{
		0:     "0.00%",
		0.5:   "0.50%",
		5.25:  "5.25%",
		12:    "12.0%",
		99.96: "100.0%",
		2000:  "999.9%",
		-3:    "0.00%",
	}
	for in, want := range cases {
		if got := formatCPU(in); got != want {
			tt.Errorf("formatCPU(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatMem(tt *testing.T) {
	if got := formatMem(0, 0); got != "0B/—" {
		tt.Errorf("formatMem(0,0) = %q", got)
	}
	if got := formatMem(1536, 1024); got != "1.5K/1.0K" {
		tt.Errorf("formatMem(1536,1024) = %q", got)
	}
	if got := formatMem(330<<20, 16<<30); got != "330.0M/16.0G" {
		tt.Errorf("formatMem(330MiB,16GiB) = %q", got)
	}
	if got := formatBytes(999); got != "999B" {
		tt.Errorf("formatBytes(999) = %q", got)
	}
}

func TestCPUPercentFrom(tt *testing.T) {
	s := docker.Stats{CPUNano: 200, SystemNano: 2000, OnlineCPUs: 4}
	prev := &statsSample{cpu: 100, sys: 1000}
	want := 100 * 100.0 / 1000.0 * 4
	if got := cpuPercentFrom(s, prev); abs(got-want) > 1e-9 {
		tt.Errorf("cpuPercentFrom = %v, want %v", got, want)
	}
	if got := cpuPercentFrom(s, nil); got != 0 {
		tt.Errorf("cpuPercentFrom(nil prev) = %v, want 0", got)
	}
	if got := cpuPercentFrom(docker.Stats{OnlineCPUs: 4}, prev); got != 0 {
		tt.Errorf("cpuPercentFrom no delta = %v, want 0", got)
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func TestContainerListRowMetricsColumns(tt *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	tt.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	m := New(nil)
	m.width = 140
	m.height = 30
	m.ready = true
	m.loading = false
	m.containers = makeTestContainers(3)
	m.fitMainViewport()

	hdr, rows := m.renderContainerList(innerW(m.width), innerW(m.width)-1, m.height-tabBarHeight-helpBarHeight)
	if !strings.Contains(hdr, "CPU %") || !strings.Contains(hdr, "MEM") {
		tt.Errorf("header missing metrics columns: %q", hdr)
	}
	plain := strings.Split(stripANSI(rows), "\n")

	// Without snapshots every metrics cell is a dash at its fixed offset.
	for i, line := range plain {
		runes := []rune(line)
		if string(runes[cpuCol:cpuCol+7]) != "      —" {
			tt.Errorf("row %d cpu dash at %d: %q", i, cpuCol, string(runes[cpuCol:cpuCol+7]))
		}
		if string(runes[memCol:memCol+13]) != "            —" {
			tt.Errorf("row %d mem dash at %d: %q", i, memCol, string(runes[memCol:memCol+13]))
		}
	}

	// With a snapshot the running container shows live values.
	first := m.containers[0].ID
	m.stats[first] = docker.Stats{ID: first, CPUPercent: 5.25, MemUsage: 330 << 20, MemLimit: 16 << 30}
	_, rows = m.renderContainerList(innerW(m.width), innerW(m.width)-1, m.height-tabBarHeight-helpBarHeight)
	line := strings.Split(stripANSI(rows), "\n")[0]
	if !strings.Contains(line, "5.25%") || !strings.Contains(line, "330.0M/16.0G") {
		tt.Errorf("running row missing metrics: %q", line)
	}
	// The used-memory value must carry the orange usage SGR, the limit must not.
	if !strings.Contains(rows, "245;167;65") {
		tt.Errorf("used memory is not painted with the usage color:\n%q", rows[:400])
	}

	// IMAGE column must land on the same column whether MEM is a short dash
	// (no data) or a full usage/limit pair, so the "image" boundary never
	// shifts between stopped and running rows.
	imgPos := func(rowRunes []rune) int {
		for i := imageCol; i < len(rowRunes); i++ {
			if rowRunes[i] != ' ' {
				return i
			}
		}
		return -1
	}
	dashRow := strings.Split(stripANSI(rows), "\n")[1] // exited row with "—"
	valRow := strings.Split(stripANSI(rows), "\n")[0]  // running row with real values
	if dp, vp := imgPos([]rune(dashRow)), imgPos([]rune(valRow)); dp != -1 && vp != -1 && dp != vp {
		tt.Errorf("IMAGE column shifted: dash row at %d, value row at %d", dp, vp)
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

func TestHLSwitchTabsWithWrap(tt *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.containers = makeTestContainers(3)
	m.selectedIdx = 2
	m.activeTab = tabContainers
	m.fitViewports()

	hl := func(m Model, r rune) Model {
		next, _ := testUpdate(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		return next
	}

	// l advances containers -> images -> volumes -> networks -> compose -> (wrap) containers
	cur := hl(m, 'l')
	if cur.activeTab != tabImages {
		tt.Fatalf("l from containers = %d, want images", cur.activeTab)
	}
	cur = hl(cur, 'l')
	if cur.activeTab != tabVolumes {
		tt.Fatalf("l from images = %d, want volumes", cur.activeTab)
	}
	cur = hl(cur, 'l')
	if cur.activeTab != tabNetworks {
		tt.Fatalf("l from volumes = %d, want networks", cur.activeTab)
	}
	cur = hl(cur, 'l')
	if cur.activeTab != tabCompose {
		tt.Fatalf("l from networks = %d, want compose", cur.activeTab)
	}
	cur = hl(cur, 'l')
	if cur.activeTab != tabContainers {
		tt.Fatalf("l from compose = %d, want wrap to containers", cur.activeTab)
	}

	// h goes backwards and wraps at the other edge
	cur = hl(cur, 'h')
	if cur.activeTab != tabCompose {
		tt.Fatalf("h from containers = %d, want wrap to compose", cur.activeTab)
	}

	// the selection always resets to the top of the new tab
	m2 := hl(m, 'l')
	if m2.selectedIdx != 0 {
		tt.Errorf("selectedIdx after l switch = %d, want 0", m2.selectedIdx)
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

func TestImageListRendersDotsAndStatusLabels(tt *testing.T) {
	m := New(nil)
	m.width = 140
	m.height = 30
	m.ready = true
	m.loading = false
	m.images = []docker.Image{
		{ID: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RepoTags: []string{"app:latest"}, Created: 1700000000, Size: 1 << 30, Containers: 2},
		{ID: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", RepoTags: []string{"busybox:latest"}, Created: 1700000000, Size: 1 << 20},
		{ID: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", RepoTags: nil, Created: 1700000000, Size: 1 << 25},
	}
	m.fitMainViewport()

	w, vw, h := innerW(m.width), innerW(m.width)-1, m.height-tabBarHeight-helpBarHeight
	hdr, rows := m.renderImageList(w, vw, h)
	_ = hdr

	lines := strings.Split(stripANSI(rows), "\n")
	if len(lines) != len(m.images) {
		tt.Fatalf("got %d rows, want %d", len(lines), len(m.images))
	}

	assertRow := func(i int, wantDot, wantLabel, wantSize, wantIDHex, wantUnit, wantDate string) {
		runes := []rune(lines[i])
		if len(runes) < 2 {
			tt.Fatalf("row %d too short: %q", i, lines[i])
		}
		if string(runes[1]) != wantDot {
			tt.Errorf("row %d dot = %q, want %q", i, string(runes[1]), wantDot)
		}
		// STATUS is the second column, flush-left at a fixed offset.
		const labelStart = 47 // [0]=indent [1]=dot [2:47] repo [47:..] status
		got := string(runes[labelStart : labelStart+len([]rune(wantLabel))])
		if got != wantLabel {
			tt.Errorf("row %d label at col %d = %q, want %q (line %q)", i, labelStart, got, wantLabel, lines[i])
		}
		// CREATED dates are flush-left in their column (aligned with the
		// header): the date begins at the left edge of the created column.
		const createdStart = 59 // createdCol(59)
		if got := string(runes[createdStart : createdStart+len([]rune(wantDate))]); got != wantDate {
			tt.Errorf("row %d created at col %d = %q, want %q (line %q)", i, createdStart, got, wantDate, lines[i])
		}
		// SIZE carries a space between value and unit.
		if !strings.Contains(lines[i], wantSize) {
			tt.Errorf("row %d missing size %q in %q", i, wantSize, lines[i])
		}
		// Units are right-aligned in the SIZE column, so they stack in one line:
		// the last rune of the SIZE column is the final unit character.
		const sizeEnd = 81 // sizeCol(72) + sizeW(9)
		if got := string(runes[sizeEnd-len([]rune(wantUnit)) : sizeEnd]); got != wantUnit {
			tt.Errorf("row %d unit at right edge = %q, want %q (line %q)", i, got, wantUnit, lines[i])
		}
		// IMAGE ID is the last column and keeps the digest prefix.
		if !strings.Contains(lines[i], "sha256:"+wantIDHex) {
			tt.Errorf("row %d missing sha256:%s id in %q", i, wantIDHex, lines[i])
		}
	}

	assertRow(0, "●", "IN-USE", "1.0 GB", "aaaa", "GB", "2023-11-15")
	assertRow(1, "●", "UNUSED", "1.0 MB", "bbbb", "MB", "2023-11-15")
	assertRow(2, "○", "DANGLING", "32.0 MB", "cccc", "MB", "2023-11-15")
}

func TestVolumeListRendersDotsAndStatusLabels(tt *testing.T) {
	// lipgloss downgrades to the Ascii profile when stdout is not a TTY;
	// force TrueColor so the emitted SGR sequences assert the real palette.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	tt.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	m := New(nil)
	m.width = 150
	m.height = 30
	m.ready = true
	m.loading = false
	m.volumes = []docker.Volume{
		{Name: "postgres_data", Driver: "local", Mountpoint: "/var/lib/docker/volumes/postgres_data/_data", Scope: "local", CreatedAt: "2026-01-01T08:00:00+03:00", RefCount: 2, Size: 1 << 30},
		{Name: "orphan", Driver: "local", Mountpoint: "/var/lib/docker/volumes/orphan/_data", Scope: "local", CreatedAt: "2026-01-02", RefCount: 0, Size: 1 << 20},
		{Name: "unknown", Driver: "local", Mountpoint: "/var/lib/docker/volumes/unknown/_data", Scope: "local", RefCount: -1, Size: -1},
	}
	m.fitMainViewport()

	w, vw, h := innerW(m.width), innerW(m.width)-1, m.height-tabBarHeight-helpBarHeight
	hdr, rows := m.renderVolumeList(w, vw, h)
	if !strings.Contains(hdr, "STATUS") {
		tt.Errorf("header lacks a STATUS column: %q", stripANSI(hdr))
	}
	if !strings.Contains(hdr, "SIZE") {
		tt.Errorf("header lacks a SIZE column: %q", stripANSI(hdr))
	}
	if !strings.Contains(hdr, "CREATED") {
		tt.Errorf("header lacks a CREATED column: %q", stripANSI(hdr))
	}
	if strings.Contains(hdr, "DRIVER") || strings.Contains(hdr, "MOUNTPOINT") || strings.Contains(hdr, "SCOPE") {
		tt.Errorf("header still lists a dropped column: %q", stripANSI(hdr))
	}

	lines := strings.Split(stripANSI(rows), "\n")
	if len(lines) != len(m.volumes) {
		tt.Fatalf("got %d rows, want %d", len(lines), len(m.volumes))
	}

	// Fixed offsets of the row format (" %s  %-64s %-9s  %10s   %-12s").
	const (
		statusCol  = 69 // STATUS label begins here
		sizeEnd    = 90 // last rune of the right-aligned SIZE column
		createdCol = 93 // CREATED begins here
		createdW   = 12 // CREATED column width
	)
	assertRow := func(i int, wantDot, wantLabel, wantSize, wantUnit, wantDate string) {
		runes := []rune(lines[i])
		if len(runes) < 2 {
			tt.Fatalf("row %d too short: %q", i, lines[i])
		}
		if string(runes[1]) != wantDot {
			tt.Errorf("row %d dot = %q, want %q", i, string(runes[1]), wantDot)
		}
		got := string(runes[statusCol : statusCol+len([]rune(wantLabel))])
		if got != wantLabel {
			tt.Errorf("row %d label at col %d = %q, want %q (line %q)", i, statusCol, got, wantLabel, lines[i])
		}
		if !strings.Contains(lines[i], wantSize) {
			tt.Errorf("row %d missing size %q in %q", i, wantSize, lines[i])
		}
		// SIZE is right-aligned, so units stack in one vertical line at the
		// right edge of the column.
		if got := string(runes[sizeEnd-len([]rune(wantUnit)) : sizeEnd]); wantUnit != "" && got != wantUnit {
			tt.Errorf("row %d unit at right edge = %q, want %q (line %q)", i, got, wantUnit, lines[i])
		}
		got = string(runes[createdCol : createdCol+createdW])
		if !strings.HasPrefix(got, wantDate) {
			tt.Errorf("row %d created at col %d = %q, want prefix %q (line %q)", i, createdCol, got, wantDate, lines[i])
		}
		// the dropped columns must no longer be part of a row
		if strings.Contains(lines[i], "/var/lib/docker/volumes") {
			tt.Errorf("row %d still renders a mountpoint in %q", i, lines[i])
		}
	}

	assertRow(0, "●", "IN-USE", "1.0 GB", "GB", "2026-01-01")
	assertRow(1, "○", "UNUSED", "1.0 MB", "MB", "2026-01-02")
	assertRow(2, "○", "UNUSED", "n/a", "", "—")

	// IN-USE renders with the success green, UNUSED with the muted gray (both
	// on the row background; the borderless test pattern uses the bare code).
	if !strings.Contains(rows, "38;2;63;185;80") {
		tt.Errorf("IN-USE status should be success green (63;185;80):\n%s", rows)
	}
	if !strings.Contains(rows, "38;2;139;147;158") {
		tt.Errorf("UNUSED status should be muted gray (139;147;158):\n%s", rows)
	}
}

func TestFormatImageSizeSeparatesValueAndUnit(tt *testing.T) {
	cases := map[int64]string{
		(1 << 40) + (1 << 39): "1.5 TB",
		(1 << 30) + (1<<30)/2: "1.5 GB",
		(1 << 20) + (1<<20)/2: "1.5 MB",
		(1 << 10) + (1<<10)/2: "1.5 KB",
		12:                    "12 B",
		0:                     "0 B",
	}
	for in, want := range cases {
		if got := formatImageSize(in); got != want {
			tt.Errorf("formatImageSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestSizeUnitColor(tt *testing.T) {
	for _, unit := range []string{"B", "KB", "MB", "GB", "TB", "PB"} {
		if got := sizeUnitColor(unit); got != t.Muted {
			tt.Errorf("sizeUnitColor(%q) = %v, want muted %v", unit, got, t.Muted)
		}
	}
}

func TestFormatImageIDKeepsPrefixAndTruncatesHex(tt *testing.T) {
	id := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	want := "sha256:0123456789ab" // 7-char prefix + 12 hex
	if got := formatImageID(id); got != want {
		tt.Errorf("formatImageID = %q, want %q", got, want)
	}
	// a bare id (no digest scheme) is truncated to the column width
	if got := formatImageID("0123456789abcdef"); got != "0123456789abcdef" {
		tt.Errorf("formatImageID(bare) = %q", got)
	}
}

func TestImageDisplayName(tt *testing.T) {
	longID := "sha256:" + strings.Repeat("a", 64)
	tagged := docker.Image{ID: longID, RepoTags: []string{"nginx:latest"}}
	if got := imageDisplayName(tagged); got != "nginx:latest" {
		tt.Errorf("tagged: %q, want nginx:latest", got)
	}
	dangling := docker.Image{ID: longID}
	if got := imageDisplayName(dangling); got != "sha256:aaaaaaaaaaaa" {
		tt.Errorf("dangling: %q, want sha256:aaaaaaaaaaaa", got)
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
	if strings.Contains(next.containerLogContent, "2026-") {
		tt.Errorf("stored log buffer must drop the RFC3339Nano prefix: %q", next.containerLogContent)
	}
	if !strings.Contains(next.containerLogContent, "13:07:03") {
		tt.Errorf("stored log buffer should carry the compact HH:MM:SS time: %q", next.containerLogContent)
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

func TestReformatLogTimestamps(tt *testing.T) {
	cases := []struct{ in, want string }{
		{
			"2026-08-28T13:07:03.100000001Z alpha\n2026-08-28T13:07:03.200000001Z beta\n",
			"13:07:03 alpha\n13:07:03 beta\n",
		},
		// bare timestamp line (no text after it) becomes just the time
		{"2026-08-28T13:07:03.100000001Z\n", "13:07:03\n"},
		// lines without a docker timestamp are left untouched
		{"just some text\nmore text\n", "just some text\nmore text\n"},
		{"", ""},
	}
	for _, c := range cases {
		if got := reformatLogTimestamps(c.in); got != c.want {
			tt.Errorf("reformatLogTimestamps(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLogTimestampLen(tt *testing.T) {
	if n := logTimestampLen("13:07:03 alpha"); n != 9 {
		tt.Errorf("logTimestampLen(hh:mm:ss line) = %d, want 9", n)
	}
	if n := logTimestampLen("no time here"); n != 0 {
		tt.Errorf("logTimestampLen(no prefix) = %d, want 0", n)
	}
	if n := logTimestampLen("13:07:03"); n != 0 { // no trailing space
		tt.Errorf("logTimestampLen(bare time) = %d, want 0", n)
	}
	if n := logTimestampLen("13:07 03 y"); n != 0 { // not HH:MM:SS shape
		tt.Errorf("logTimestampLen(malformed) = %d, want 0", n)
	}
}

func TestStyleLogRowsMutesTimestamp(tt *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	tt.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	content := "13:07:03 alpha\n13:07:04 beta"
	out := styleLogRows(content, textSel{}, 20, 10, 0)
	lines := strings.Split(out, "\n")
	// The timestamp digits must carry the muted SGR, the message must not.
	first := lines[0]
	muted := "38;2;139;147;158" // t.Muted = #8b949e
	if !strings.Contains(first, muted) {
		tt.Errorf("timestamp not muted in: %q", first)
	}
	if strings.Contains(styleLogRows("alpha\nbeta", textSel{}, 20, 10, 0), muted) {
		tt.Errorf("plain log line should not be muted")
	}
	_ = lines
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
	press := tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 14}
	next := testMouseUpdate(m, press)
	if !next.detailDragSel {
		tt.Fatal("press in the Info body should start a drag")
	}
	next = testMouseUpdate(next, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, X: 30, Y: 16})
	if next.detailSel.endR != 3 {
		tt.Errorf("Info drag end row = %d, want 3", next.detailSel.endR)
	}
	if next.detailSel.endC < 0 || next.detailSel.endC >= len([]rune(strings.Split(next.detailContent, "\n")[3])) {
		tt.Errorf("Info drag end col = %d out of row bounds", next.detailSel.endC)
	}
	next, cmd := testUpdate(next, tea.MouseMsg{Type: tea.MouseRelease, Action: tea.MouseActionRelease, X: 30, Y: 16})
	if next.detailSel.active {
		tt.Fatal("release should clear the Info selection (text was copied)")
	}
	if cmd == nil {
		tt.Error("non-trivial Info drag should auto-copy via OSC 52")
	}

	// a plain click clears the Info selection
	next = testMouseUpdate(detailTestModel(), tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 14})
	next, cmd = testUpdate(next, tea.MouseMsg{Type: tea.MouseRelease, Action: tea.MouseActionRelease, X: 1, Y: 14})
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

func TestStyleLogRowsKeepsTimeSpace(tt *testing.T) {
	withTrueColor(tt, func() {
		line := "16:46:11 value.deserializer = class X"
		// Non-selected: exactly one space must separate time from text.
		out := styleLogRows(line, textSel{}, 120, 4, 0)
		plain := strings.TrimRight(stripANSI(strings.Split(out, "\n")[0]), " ")
		if plain != line {
			tt.Errorf("unselected row altered text: %q, want %q", plain, line)
		}
		// Selected across a middle band: the space must still be there.
		sel := textSel{active: true, anR: 0, anC: 12, endR: 0, endC: 20}
		out2 := styleLogRows(line, sel, 120, 4, 0)
		plain2 := strings.TrimRight(stripANSI(strings.Split(out2, "\n")[0]), " ")
		if plain2 != line {
			tt.Errorf("selected row altered text: %q, want %q", plain2, line)
		}
	})
}

func TestLogDragSelectsAcrossRows(tt *testing.T) {
	m := logTestModel()
	// viewport row 0 == buffer row 1 (YOffset=1), content band starts at y=21
	press := tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 15}
	_ = press
	next := testMouseUpdate(m, press)
	if !next.dragSel {
		tt.Fatal("left press in the log body should start a drag")
	}
	if next.logSel.anR != 1 || next.logSel.anC != 0 {
		tt.Errorf("anchor = (%d,%d), want buffer (1,0)", next.logSel.anR, next.logSel.anC)
	}

	motion := tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, X: 4, Y: 17}
	next = testMouseUpdate(next, motion)
	if next.logSel.endR != 3 || next.logSel.endC != 3 {
		tt.Errorf("end = (%d,%d), want (3,3)", next.logSel.endR, next.logSel.endC)
	}

	release := tea.MouseMsg{Type: tea.MouseRelease, Action: tea.MouseActionRelease, X: 4, Y: 17}
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

	press := tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 16}
	next := testMouseUpdate(m, press)
	if !next.dragSel {
		tt.Fatal("press should start a fresh drag")
	}
	release := tea.MouseMsg{Type: tea.MouseRelease, Action: tea.MouseActionRelease, X: 1, Y: 16}
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

func TestTabSwitchesInfoLogs(tt *testing.T) {
	m := logTestModel()
	m.activeSubTab = subTabInfo

	tabMsg := tea.KeyMsg{Type: tea.KeyTab}
	next := testMouseUpdate(m, tabMsg)
	if next.activeSubTab != subTabLogs {
		tt.Fatalf("tab from Info = %d, want Logs", next.activeSubTab)
	}
	next = testMouseUpdate(next, tabMsg)
	if next.activeSubTab != subTabInfo {
		tt.Fatalf("tab from Logs = %d, want Info", next.activeSubTab)
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
	press := tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 15}
	next := testMouseUpdate(m, press)
	if !next.dragSel {
		tt.Fatal("press should start a drag")
	}
	next = testMouseUpdate(next, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, X: 4, Y: 16})
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
	next = testMouseUpdate(next, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, X: 4, Y: 17})
	if next.logSel.endR != 3 {
		tt.Errorf("motion after tick end row = %d, want 3", next.logSel.endR)
	}
	next, cmd := testUpdate(next, tea.MouseMsg{Type: tea.MouseRelease, Action: tea.MouseActionRelease, X: 4, Y: 17})
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
	press := tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 15}
	next := testMouseUpdate(m, press)
	if !next.dragSel {
		tt.Fatal("press should start a drag")
	}
	next = testMouseUpdate(next, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, X: 4, Y: 17})
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
	next := testMouseUpdate(m, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 15})
	next = testMouseUpdate(next, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, X: 4, Y: 17})
	next, cmd := testUpdate(next, tea.MouseMsg{Type: tea.MouseRelease, Action: tea.MouseActionRelease, X: 4, Y: 17})
	if next.logSel.active {
		tt.Fatal("release of a real drag should clear the selection (text was copied)")
	}
	if cmd == nil {
		tt.Error("non-trivial release should return an OSC 52 copy command")
	}
	// a plain click (release on the same cell) must not auto-copy
	next = testMouseUpdate(logTestModel(), tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 15})
	next, cmd = testUpdate(next, tea.MouseMsg{Type: tea.MouseRelease, Action: tea.MouseActionRelease, X: 1, Y: 15})
	if next.logSel.active {
		tt.Fatal("plain click must not finalize an active selection")
	}
	if cmd != nil {
		tt.Error("plain click should not copy to the clipboard")
	}
}

func TestCopyKeyCopiesFinalizedSelection(tt *testing.T) {
	m := logTestModel()
	next := testMouseUpdate(m, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 15})
	next = testMouseUpdate(next, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, X: 4, Y: 16})
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
	next2 := testMouseUpdate(m, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 15})
	next2 = testMouseUpdate(next2, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, X: 4, Y: 16})
	next2 = testMouseUpdate(next2, tea.MouseMsg{Type: tea.MouseRelease, Action: tea.MouseActionRelease, X: 4, Y: 16})
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
	press := tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 15}
	motion := tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, X: 4, Y: 17}
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
	press := tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 15}
	next := testMouseUpdate(m, press)
	if !next.dragSel {
		tt.Fatal("press should start a drag")
	}
	next = testMouseUpdate(next, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, X: 4, Y: 17})

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
	press := tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 15}
	next := testMouseUpdate(m, press)
	gen0 := next.dragGen
	next = testMouseUpdate(next, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, X: 4, Y: 17})

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
	next = testMouseUpdate(detailTestModel(), tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 14})
	next, cmd = testUpdate(next, tea.MouseMsg{Type: tea.MouseRelease, Action: tea.MouseActionRelease, X: 1, Y: 14})
	if cmd != nil || next.detailDragSel {
		tt.Error("plain Info click must not copy or leave a drag")
	}
}

// TestInfoDragTimeoutFinalizes mirrors the Logs timeout for the Info pane.
func TestInfoDragTimeoutFinalizes(tt *testing.T) {
	m := detailTestModel()
	press := tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 14}
	next := testMouseUpdate(m, press)
	if !next.detailDragSel {
		tt.Fatal("press in the Info body should start a drag")
	}
	next = testMouseUpdate(next, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, X: 30, Y: 16})

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
	next := testMouseUpdate(m, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 14})
	next = testMouseUpdate(next, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, X: 30, Y: 16})
	released, cmd := testUpdate(next, tea.MouseMsg{Type: tea.MouseRelease, Action: tea.MouseActionRelease, X: 30, Y: 16})
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
	next := testMouseUpdate(m, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 15})
	next = testMouseUpdate(next, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, X: 4, Y: 16})
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
	next := testMouseUpdate(m, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: 1, Y: 14})
	cl, cmd := testUpdate(next, tea.MouseMsg{Type: tea.MouseRelease, Action: tea.MouseActionRelease, X: 1, Y: 14})
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
	wantedTitle := "Actions for container " + containerDisplayName(m.containers[0])
	if got := strings.TrimSpace(strings.Trim(stripANSI(rows[1]), "│")); got != wantedTitle {
		tt.Errorf("title = %q, want %q", got, wantedTitle)
	}
	// the popup title is centered within the title row (borders are the cell
	// inside each border plus the equal centering pad; a 1-cell slack hangs
	// on the right when the interior width is odd)
	if text := strings.Trim(stripANSI(rows[1]), "│"); text != "" {
		idx := strings.Index(text, wantedTitle)
		if idx < 0 {
			tt.Errorf("title %q not found in row %q", wantedTitle, text)
		} else if gap := (len(text) - idx - len(wantedTitle)) - idx; gap > 1 || gap < 0 {
			tt.Errorf("title not centered: left=%d right=%d in %q", idx, len(text)-idx-len(wantedTitle), text)
		}
	}
	// the title is separated from the actions by a full-width horizontal rule
	if got := strings.TrimSpace(strings.Trim(stripANSI(rows[2]), "│")); strings.ReplaceAll(got, "─", "") != "" {
		tt.Errorf("separator row = %q, want a full-width ─ rule", got)
	}
	// running container -> 7 items: Stop, Pause, Exec shell, Attach,
	// Restart, Remove(submenu), plus the bulk Prune stopped separated by a rule
	if got := strings.Fields(strings.Trim(strings.Trim(stripANSI(rows[3]), "│"), " "))[:2]; len(got) != 2 || got[0] != "s" || got[1] != "Stop" {
		tt.Errorf("first item = %q, want a s Stop row for a running container", strings.TrimSpace(strings.Trim(stripANSI(rows[3]), "│")))
	}
	if got := strings.Fields(strings.Trim(strings.Trim(stripANSI(rows[4]), "│"), " "))[:2]; len(got) != 2 || got[0] != "p" || got[1] != "Pause" {
		tt.Errorf("second item = %q, want a p Pause row for a running container", strings.TrimSpace(strings.Trim(stripANSI(rows[4]), "│")))
	}
	if got := strings.Fields(strings.Trim(strings.Trim(stripANSI(rows[5]), "│"), " "))[:2]; len(got) != 2 || got[0] != "e" || got[1] != "Exec" {
		tt.Errorf("third item = %q, want an e Exec shell row for a running container", strings.TrimSpace(strings.Trim(stripANSI(rows[5]), "│")))
	}
	if got := strings.Fields(strings.Trim(strings.Trim(stripANSI(rows[6]), "│"), " "))[:2]; len(got) != 2 || got[0] != "t" || got[1] != "Attach" {
		tt.Errorf("fourth item = %q, want a t Attach row for a running container", strings.TrimSpace(strings.Trim(stripANSI(rows[6]), "│")))
	}
	if got := strings.Fields(strings.Trim(strings.Trim(stripANSI(rows[7]), "│"), " "))[:2]; len(got) != 2 || got[0] != "r" || got[1] != "Restart" {
		tt.Errorf("fifth item = %q, want a r Restart row", strings.TrimSpace(strings.Trim(stripANSI(rows[7]), "│")))
	}
	if got := strings.Trim(strings.Trim(stripANSI(rows[8]), "│"), " "); !strings.HasPrefix(got, "d Remove ›") {
		tt.Errorf("sixth item = %q, want a d Remove submenu row", got)
	}
	// a horizontal rule separates the single-container actions from the bulk
	// "Prune stopped" action below (a stopped container exists in the list)
	if got := strings.TrimSpace(strings.Trim(stripANSI(rows[9]), "│")); strings.ReplaceAll(got, "─", "") != "" {
		tt.Errorf("divider row = %q, want a full-width ─ rule", got)
	}
	if got := strings.Trim(strings.Trim(stripANSI(rows[10]), "│"), " "); !strings.HasPrefix(got, "g Prune stopped") {
		tt.Errorf("seventh item = %q, want a g Prune stopped row", strings.TrimSpace(strings.Trim(stripANSI(rows[10]), "│")))
	}
	if len(rows) != 12 {
		tt.Errorf("menu rows = %d, want 12 (7 items + divider + header + borders)", len(rows))
	}

	// the docker CLI hint column is right-aligned: every hint ends at the same
	// display column, flush against the right border of the menu. The "Remove ›"
	// submenu row carries no hint, so it is skipped.
	cliEnd := -1
	for _, r := range rows[3:11] {
		text := strings.Trim(stripANSI(r), "│")
		idx := strings.LastIndex(text, "docker")
		if idx < 0 {
			continue
		}
		if end := lipgloss.Width(stripANSI(r)) - (len(text) - idx); cliEnd == -1 {
			cliEnd = end
		} else if end != cliEnd {
			tt.Errorf("docker hints not right-aligned: end %d vs %d in %q", end, cliEnd, text)
		}
	}
	// the hint is muted (#8b949e) on the unselected rows, but is NOT muted on
	// the selected row: gray-on-green is hard to read, so the selected hint is
	// bright white like the hotkey
	if !strings.Contains(rows[4], "38;2;139;147;158;48;2;22;27;34mdocker pause ") {
		tt.Error("unselected-row docker hint must be muted (139;147;158)")
	}
	if strings.Contains(rows[3], "38;2;139;147;158;48;2;46;160;67mdocker stop ") {
		tt.Error("selected-row docker hint must not be muted (gray on green is unreadable)")
	}
	if !strings.Contains(rows[3], "38;2;230;237;243;48;2;46;160;67mdocker stop ") {
		tt.Error("selected-row docker hint must be bright white (230;237;243)")
	}

	// the popup title is orange (#f0883e)
	if !strings.Contains(rows[1], "38;2;240;136;62") {
		tt.Errorf("menu title should be orange (240;136;62):\n%s", rows[1])
	}

	// the hotkey on the selected row is white (readable on the green
	// background); the hotkeys on the unselected rows are accent green
	if !strings.Contains(rows[3], "38;2;230;237;243") {
		tt.Errorf("selected row hotkey should be bright white (230;237;243):\n%s", rows[3])
	}
	if !strings.Contains(rows[4], "38;2;63;185;80") {
		tt.Errorf("unselected row hotkey should be accent green (63;185;80):\n%s", rows[4])
	}

	// the selected row (sel=0) is inverted with the accent background; the
	// other rows are not
	activeBG := ";48;2;46;160;67m"
	if !strings.Contains(rows[3], activeBG) {
		tt.Error("selected menu row must carry the accent background")
	}
	for _, r := range rows[4:7] {
		if strings.Contains(r, activeBG) {
			tt.Error("unselected menu row must not carry the accent background")
		}
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
	c := m.containers[m.selectedIdx]
	m.enterConfirmStage("Remove "+containerDisplayName(c)+"?", m.removeContainer)
	crows := m.renderContainerMenu()
	if len(crows) != m.menu.h {
		tt.Errorf("confirm rows = %d, want %d", len(crows), m.menu.h)
	}
	wantHeader := "Remove " + containerDisplayName(m.containers[m.selectedIdx]) + "?"
	if got := strings.TrimSpace(strings.Trim(stripANSI(crows[1]), "│")); got != wantHeader {
		tt.Errorf("confirm header = %q, want %q", got, wantHeader)
	}
	if got := strings.TrimSpace(strings.Trim(stripANSI(crows[2]), "│")); strings.ReplaceAll(got, "─", "") != "" {
		tt.Errorf("confirm separator row = %q, want a full-width ─ rule", got)
	}
	if got := strings.TrimSpace(strings.Trim(stripANSI(crows[3]), "│")); got != "Yes, remove" {
		tt.Errorf("confirm yes = %q", got)
	}
	if !strings.Contains(crows[3], activeBG) {
		tt.Error("confirm resets the cursor to the first (Yes, remove) item")
	}
	if got := strings.TrimSpace(strings.Trim(stripANSI(crows[4]), "│")); got != "No, cancel" {
		tt.Errorf("confirm no = %q", got)
	}

	// the "Remove with data" variant: same confirm layout, its own header and
	// the Yes item still selected
	m.enterConfirmStage("Remove "+containerDisplayName(c)+" and its volumes?", m.removeContainerVolumes)
	drows := m.renderContainerMenu()
	if len(drows) != m.menu.h {
		tt.Errorf("data-confirm rows = %d, want %d", len(drows), m.menu.h)
	}
	wantDataHeader := "Remove " + containerDisplayName(m.containers[m.selectedIdx]) + " and its volumes?"
	if got := strings.TrimSpace(strings.Trim(stripANSI(drows[1]), "│")); got != wantDataHeader {
		tt.Errorf("data-confirm header = %q, want %q", got, wantDataHeader)
	}
	if !strings.Contains(drows[3], activeBG) {
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
	if next.menu.sel != 3 {
		tt.Errorf("down = %d, want 3", next.menu.sel)
	}
	next = testMouseUpdate(next, down)
	if next.menu.sel != 4 {
		tt.Errorf("down = %d, want 4", next.menu.sel)
	}
	next = testMouseUpdate(next, down)
	if next.menu.sel != 5 {
		tt.Errorf("down = %d, want 5", next.menu.sel)
	}
	next = testMouseUpdate(next, down)
	if next.menu.sel != 6 {
		tt.Errorf("down = %d, want 6", next.menu.sel)
	}
	next = testMouseUpdate(next, down)
	if next.menu.sel != 6 {
		tt.Errorf("down past the last item = %d, want 6 (clamped)", next.menu.sel)
	}
	next = testMouseUpdate(next, up)
	if next.menu.sel != 5 {
		tt.Errorf("up = %d, want 5", next.menu.sel)
	}
	next = testMouseUpdate(next, up)
	if next.menu.sel != 4 {
		tt.Errorf("up = %d, want 4", next.menu.sel)
	}
	next = testMouseUpdate(next, up)
	if next.menu.sel != 3 {
		tt.Errorf("up = %d, want 3", next.menu.sel)
	}
	next = testMouseUpdate(next, up)
	if next.menu.sel != 2 {
		tt.Errorf("up = %d, want 2", next.menu.sel)
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
	next, _ = testUpdate(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("z")})
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

	// left-click on the Remove item (running: title + separator + Stop + Pause
	// above it, plus the top border) opens the Remove submenu and keeps the
	// popup open in the first stage
	itemY := m.menu.y + 8 + 1 // 0-based row -> 1-based mouse Y
	itemX := m.menu.x + 3 + 1
	next := testMouseUpdate(m, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: itemX, Y: itemY})
	if !next.menuOpen {
		tt.Fatal("click on the Remove item should keep the popup open")
	}
	if next.menu.confirm {
		tt.Fatal("Remove activation should open the submenu, not the confirm stage")
	}
	if len(next.menu.items) != 2 {
		tt.Fatalf("Remove submenu have %d items, want 2", len(next.menu.items))
	}
	if got := next.menu.items[0].label; got != "Remove" {
		tt.Errorf("submenu first = %q, want Remove", got)
	}
	if got := next.menu.items[1].label; got != "Remove with data" {
		tt.Errorf("submenu second = %q, want Remove with data", got)
	}
	// Esc backs out of the submenu to the root without closing
	next = testMouseUpdate(next, tea.KeyMsg{Type: tea.KeyEsc})
	if !next.menuOpen {
		tt.Fatal("Esc in the submenu should return to the root, not close")
	}
	if len(next.menu.items) != 7 {
		tt.Fatalf("back to root should restore %d items, got %d", 7, len(next.menu.items))
	}

	// left-click clearly outside the box (right and below) closes the popup;
	// measure against next's geometry
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

	// open the Remove submenu via the d hotkey
	openSub := func(m Model) Model {
		next, _ := testUpdate(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
		return next
	}

	// plain Remove: open submenu, Enter on Remove, Enter confirms Yes
	next := openSub(openAt(0))
	if len(next.menu.items) != 2 {
		tt.Fatalf("submenu items = %d, want 2", len(next.menu.items))
	}
	next, cmd := testUpdate(next, enter)
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
	next = openSub(openAt(0))
	next, _ = testUpdate(next, enter)
	next, cmd = testUpdate(next, tea.KeyMsg{Type: tea.KeyEsc})
	if next.menuOpen {
		tt.Fatal("Esc in the confirm should close the popup")
	}
	if cmd != nil {
		tt.Error("Esc must not dispatch a command")
	}

	// "No, cancel" closes the popup without dispatching
	next = openSub(openAt(0))
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

	// Remove with data: open submenu, down to it, Enter, confirm header
	// mentions volumes
	next = openSub(openAt(0))
	next, _ = testUpdate(next, down)
	if got := next.menu.items[next.menu.sel].label; got != "Remove with data" {
		tt.Fatalf("down in submenu = %d (%q)", next.menu.sel, got)
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

	// once drilled into the submenu, hotkeys are gone: d does nothing there
	next = openSub(openAt(0))
	if got := next.menu.items[0].key; got != "" {
		tt.Fatalf("submenu should carry no hotkeys, got %q", got)
	}
	next, _ = testUpdate(next, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if next.menu.confirm {
		tt.Fatal("a stray d inside the submenu should not stage a confirm")
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
	if got := next.menu.items[2].label; got != "Remove" {
		tt.Errorf("third item = %q, want the Remove submenu", got)
	}
	if got := next.menu.items[2].children[1].label; got != "Remove with data" {
		tt.Errorf("Remove submenu second = %q, want Remove with data", got)
	}
	if got := next.menu.header; got != "Actions for container test-container-2" {
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
	if got := next.menu.header; got != "Actions for container test-container-1" {
		tt.Errorf("menu title after k = %q", got)
	}
	if want := max((next.width-next.menu.w)/2, 0); next.menu.x != want {
		tt.Errorf("popup x after k = %d, want centered %d", next.menu.x, want)
	}

	// x on another tab with no rows must not open the popup
	m.activeTab = tabImages
	next, _ = testUpdate(m, x)
	if next.menuOpen {
		tt.Fatal("x must not open the popup on an empty Images tab")
	}
	m.activeTab = tabVolumes
	next, _ = testUpdate(m, x)
	if next.menuOpen {
		tt.Fatal("x must not open the popup on the Volumes tab")
	}
}

func TestKeyXOpensImageMenu(tt *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.images = []docker.Image{
		{ID: "sha256:" + strings.Repeat("a", 64), RepoTags: []string{"app:latest"}, Containers: 2},
		{ID: "sha256:" + strings.Repeat("b", 64), RepoTags: []string{"busybox:latest"}, Containers: 0},
	}
	m.activeTab = tabImages
	m.selectedIdx = 1
	m.fitViewports()

	x := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}
	next, cmd := testUpdate(m, x)
	if !next.menuOpen {
		tt.Fatal("x should open the popup on the selected image")
	}
	if cmd != nil {
		tt.Error("x must not dispatch an action")
	}
	if got := next.menu.header; got != "Actions for image busybox:latest" {
		tt.Errorf("menu title = %q", got)
	}
	if got := next.menu.items[0].label; got != "Remove" {
		tt.Errorf("first item = %q, want Remove for an unused image", got)
	}
	if want := max((next.width-next.menu.w)/2, 0); next.menu.x != want {
		tt.Errorf("popup x = %d, want centered %d", next.menu.x, want)
	}
}

func TestImageMenuVariesByStatus(tt *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.activeTab = tabImages

	// IN-USE images cannot be removed plainly: only "Force remove" is offered.
	m.images = []docker.Image{{ID: "sha256:" + strings.Repeat("a", 64), RepoTags: []string{"app:latest"}, Containers: 1}}
	m.fitViewports()
	items, _ := m.imageMenuItems(m.images[0])
	if got := items[0].label; got != "Force remove" {
		tt.Errorf("in-use item = %q, want Force remove", got)
	}

	// UNUSED images offer a plain Remove.
	m.images = []docker.Image{{ID: "sha256:" + strings.Repeat("a", 64), RepoTags: []string{"app:latest"}, Containers: 0}}
	m.fitViewports()
	items, _ = m.imageMenuItems(m.images[0])
	if got := items[0].label; got != "Remove" {
		tt.Errorf("unused item = %q, want Remove", got)
	}
}

func TestImageMenuPruneOnlyWithDangling(tt *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.activeTab = tabImages

	labelled := []menuItem{}
	m.images = []docker.Image{
		{ID: "sha256:" + strings.Repeat("a", 64), RepoTags: []string{"app:latest"}},
		{ID: "sha256:" + strings.Repeat("b", 64), RepoTags: []string{"busybox:latest"}},
	}
	m.fitViewports()
	labelled, _ = m.imageMenuItems(m.images[0])
	if len(labelled) != 1 {
		tt.Fatalf("menu without dangling = %d items, want 1", len(labelled))
	}

	// one dangling image unlocks the prune action
	m.images = append(m.images, docker.Image{ID: "sha256:" + strings.Repeat("c", 64), RepoTags: nil})
	m.fitViewports()
	items, dividers := m.imageMenuItems(m.images[0])
	if len(items) != 2 {
		tt.Fatalf("menu with dangling = %d items, want 2", len(items))
	}
	if got := items[1].label; got != "Prune dangling" {
		tt.Errorf("second item = %q, want Prune dangling", got)
	}
	if !items[1].confirm {
		tt.Error("Prune dangling must stage a confirm")
	}
	// the bulk action is visually separated from the single-image Remove
	if len(dividers) != 1 || dividers[0] != 1 {
		tt.Errorf("dividers = %v, want [1] (before the prune item)", dividers)
	}
}

func TestImageMenuConfirmFlow(tt *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.activeTab = tabImages
	m.images = []docker.Image{
		{ID: "sha256:" + strings.Repeat("a", 64), RepoTags: []string{"app:latest"}, Containers: 2},
		{ID: "sha256:" + strings.Repeat("b", 64), RepoTags: []string{"busybox:latest"}, Containers: 0},
	}
	m.selectedIdx = 1
	m.fitViewports()

	x := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}
	next := testMouseUpdate(m, x)

	// Enter on the first (Remove) row enters the destructive confirm stage
	enter := tea.KeyMsg{Type: tea.KeyEnter}
	next, _ = testUpdate(next, enter)
	if !next.menu.confirm {
		tt.Fatal("Enter on Remove should stage the confirm stage")
	}
	if got := next.menu.items[0].label; got != "Yes, remove" {
		tt.Errorf("confirm first item = %q, want Yes, remove", got)
	}
	if got := next.menu.header; got != "Remove busybox:latest?" {
		tt.Errorf("confirm header = %q", got)
	}
}

func TestKeyXOpensVolumeMenu(tt *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.volumes = []docker.Volume{
		{Name: "postgres_data", RefCount: 2, Size: 1 << 30},
		{Name: "orphan", RefCount: 0, Size: 1 << 20},
	}
	m.activeTab = tabVolumes
	m.selectedIdx = 1
	m.fitViewports()

	x := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}
	next, cmd := testUpdate(m, x)
	if !next.menuOpen {
		tt.Fatal("x should open the popup on the selected volume")
	}
	if cmd != nil {
		tt.Error("x must not dispatch an action")
	}
	if got := next.menu.header; got != "Actions for volume orphan" {
		tt.Errorf("menu title = %q", got)
	}
	if got := next.menu.items[0].label; got != "Remove" {
		tt.Errorf("first item = %q, want Remove for an unused volume", got)
	}
	if want := max((next.width-next.menu.w)/2, 0); next.menu.x != want {
		tt.Errorf("popup x = %d, want centered %d", next.menu.x, want)
	}
}

func TestVolumeMenuVariesByStatus(tt *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.activeTab = tabVolumes

	// IN-USE volumes cannot be removed plainly: only "Force remove" is offered.
	m.volumes = []docker.Volume{{Name: "postgres_data", RefCount: 2, Size: 1 << 30}}
	m.fitViewports()
	items, _ := m.volumeMenuItems(m.volumes[0])
	if got := items[0].label; got != "Force remove" {
		tt.Errorf("in-use item = %q, want Force remove", got)
	}
	if got := items[0].cli; got != "docker volume rm -f postgres_data" {
		tt.Errorf("in-use CLI = %q, want docker volume rm -f postgres_data", got)
	}
	if got := items[0].confirmHeader; got != "Force remove postgres_data?" {
		tt.Errorf("in-use confirm header = %q", got)
	}

	// UNUSED volumes offer a plain Remove.
	m.volumes = []docker.Volume{{Name: "orphan", RefCount: 0, Size: 1 << 20}}
	m.fitViewports()
	items, _ = m.volumeMenuItems(m.volumes[0])
	if got := items[0].label; got != "Remove" {
		tt.Errorf("unused item = %q, want Remove", got)
	}
	if got := items[0].cli; got != "docker volume rm orphan" {
		tt.Errorf("unused CLI = %q, want docker volume rm orphan", got)
	}
}

func TestVolumeMenuPruneOnlyWithUnused(tt *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.activeTab = tabVolumes

	// all volumes in use: only the single-volume Remove is offered
	m.volumes = []docker.Volume{
		{Name: "db", RefCount: 1, Size: 1 << 30},
		{Name: "cache", RefCount: 2, Size: 1 << 20},
	}
	m.fitViewports()
	labelled, _ := m.volumeMenuItems(m.volumes[0])
	if len(labelled) != 1 {
		tt.Fatalf("menu without unused = %d items, want 1", len(labelled))
	}

	// one unused volume unlocks the prune action
	m.volumes = append(m.volumes, docker.Volume{Name: "orphan", RefCount: 0})
	m.fitViewports()
	items, dividers := m.volumeMenuItems(m.volumes[0])
	if len(items) != 2 {
		tt.Fatalf("menu with unused = %d items, want 2", len(items))
	}
	if got := items[1].label; got != "Prune unused" {
		tt.Errorf("second item = %q, want Prune unused", got)
	}
	if got := items[1].cli; got != "docker volume prune -a" {
		tt.Errorf("prune CLI = %q, want docker volume prune -a", got)
	}
	if !items[1].confirm {
		tt.Error("Prune unused must stage a confirm")
	}
	if got := items[1].confirmHeader; got != "Prune all unused volumes?" {
		tt.Errorf("prune confirm header = %q", got)
	}
	// the bulk action is visually separated from the single-volume Remove
	if len(dividers) != 1 || dividers[0] != 1 {
		tt.Errorf("dividers = %v, want [1] (before the prune item)", dividers)
	}
}

func TestVolumeMenuConfirmFlow(tt *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.activeTab = tabVolumes
	m.volumes = []docker.Volume{
		{Name: "postgres_data", RefCount: 2},
		{Name: "orphan", RefCount: 0},
	}
	m.selectedIdx = 1
	m.fitViewports()

	m = testMouseUpdate(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	enter := tea.KeyMsg{Type: tea.KeyEnter}

	// unused volume: plain removal confirm
	next := testMouseUpdate(m, enter)
	if !next.menu.confirm {
		tt.Fatal("Enter on Remove should stage the confirm stage")
	}
	if got := next.menu.items[0].label; got != "Yes, remove" {
		tt.Errorf("confirm first item = %q, want Yes, remove", got)
	}
	if got := next.menu.header; got != "Remove orphan?" {
		tt.Errorf("confirm header = %q", got)
	}

	// in-use volume: force-remove confirm
	m2 := New(nil)
	m2.width = 120
	m2.height = 30
	m2.ready = true
	m2.activeTab = tabVolumes
	m2.volumes = []docker.Volume{{Name: "postgres_data", RefCount: 2}}
	m2.selectedIdx = 0
	m2.fitViewports()
	m2 = testMouseUpdate(m2, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	next = testMouseUpdate(m2, enter)
	if got := next.menu.header; got != "Force remove postgres_data?" {
		tt.Errorf("in-use confirm header = %q", got)
	}
}

func TestNetworkListRendersStatusLabels(tt *testing.T) {
	// lipgloss downgrades to the Ascii profile when stdout is not a TTY;
	// force TrueColor so the emitted SGR sequences assert the real palette.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	tt.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	m := New(nil)
	m.width = 150
	m.height = 30
	m.ready = true
	m.loading = false
	m.networks = []docker.Network{
		{Name: "bridge", Driver: "bridge", Scope: "local", Containers: 2},
		{Name: "web_overlay", Driver: "overlay", Scope: "global", Containers: 0},
		{Name: "unknown", Driver: "bridge", Scope: "local", Containers: -1},
	}
	m.fitMainViewport()

	w, vw, h := innerW(m.width), innerW(m.width)-1, m.height-tabBarHeight-helpBarHeight
	hdr, rows := m.renderNetworkList(w, vw, h)
	if !strings.Contains(hdr, "STATUS") {
		tt.Errorf("header lacks a STATUS column: %q", stripANSI(hdr))
	}
	if !strings.Contains(hdr, "DRIVER") {
		tt.Errorf("header lacks a DRIVER column: %q", stripANSI(hdr))
	}
	if !strings.Contains(hdr, "SCOPE") {
		tt.Errorf("header lacks a SCOPE column: %q", stripANSI(hdr))
	}
	if !strings.Contains(hdr, "NAME") {
		tt.Errorf("header lacks a NAME column: %q", stripANSI(hdr))
	}
	// the network id exposes nothing actionable in a TUI and was dropped
	if strings.Contains(hdr, "ID") {
		tt.Errorf("header still lists a dropped ID column: %q", stripANSI(hdr))
	}

	lines := strings.Split(stripANSI(rows), "\n")
	if len(lines) != len(m.networks) {
		tt.Fatalf("got %d rows, want %d", len(lines), len(m.networks))
	}

	// Fixed offsets of the row format (" %s  %-34s %-9s  %-16s  %-14s").
	const statusCol = 39 // STATUS label begins here
	assertRow := func(i int, wantDot, wantLabel string) {
		runes := []rune(lines[i])
		if len(runes) < 2 {
			tt.Fatalf("row %d too short: %q", i, lines[i])
		}
		if string(runes[1]) != wantDot {
			tt.Errorf("row %d dot = %q, want %q", i, string(runes[1]), wantDot)
		}
		got := string(runes[statusCol : statusCol+len([]rune(wantLabel))])
		if got != wantLabel {
			tt.Errorf("row %d label at col %d = %q, want %q (line %q)", i, statusCol, got, wantLabel, lines[i])
		}
	}

	assertRow(0, "●", "IN-USE")
	assertRow(1, "○", "UNUSED")
	assertRow(2, "○", "UNUSED")

	// IN-USE renders with the success green, UNUSED with the muted gray.
	if !strings.Contains(rows, "38;2;63;185;80") {
		tt.Errorf("IN-USE status should be success green (63;185;80):\n%s", rows)
	}
	if !strings.Contains(rows, "38;2;139;147;158") {
		tt.Errorf("UNUSED status should be muted gray (139;147;158):\n%s", rows)
	}
}

func TestComposeListRendersProjects(tt *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	tt.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	m := New(nil)
	m.width = 150
	m.height = 30
	m.ready = true
	m.loading = false
	m.compose = []docker.ComposeProject{
		{Name: "webtier", Services: []docker.ComposeService{
			{Name: "api", Running: true},
			{Name: "db", Running: true},
			{Name: "web", Running: false},
		}, Running: 2, Total: 3, ConfigFiles: "docker-compose.yml"},
		{Name: "emptyproj", Services: []docker.ComposeService{{Name: "worker", Running: false}}, Running: 0, Total: 1, ConfigFiles: ""},
	}
	m.fitMainViewport()

	w, vw, h := innerW(m.width), innerW(m.width)-1, m.height-tabBarHeight-helpBarHeight
	hdr, rows := m.renderComposeList(w, vw, h)
	if !strings.Contains(hdr, "PROJECT") {
		tt.Errorf("header lacks a PROJECT column: %q", stripANSI(hdr))
	}
	if !strings.Contains(hdr, "SERVICES") {
		tt.Errorf("header lacks a SERVICES column: %q", stripANSI(hdr))
	}
	if !strings.Contains(hdr, "CONFIG") {
		tt.Errorf("header lacks a CONFIG column: %q", stripANSI(hdr))
	}

	lines := strings.Split(stripANSI(rows), "\n")
	if len(lines) != len(m.compose) {
		tt.Fatalf("got %d rows, want %d", len(lines), len(m.compose))
	}

	// Fixed offsets of the row format (" %s  %-40s %-14s %-s").
	const servicesCol = 45 // SERVICES label begins here
	assertRow := func(i int, wantLabel string) {
		runes := []rune(lines[i])
		if string(runes[1]) != "▶" {
			tt.Errorf("row %d chevron = %q, want collapsed ▶", i, string(runes[1]))
		}
		// project rows carry no status dot: the chevron is the leading glyph
		if string(runes[2]) != " " || string(runes[3]) != " " {
			tt.Errorf("row %d has an unexpected glyph after the chevron: %q", i, string(runes[2:4]))
		}
		got := string(runes[servicesCol : servicesCol+len([]rune(wantLabel))])
		if got != wantLabel {
			tt.Errorf("row %d label at col %d = %q, want %q (line %q)", i, servicesCol, got, wantLabel, lines[i])
		}
	}
	assertRow(0, "2/3 up")
	assertRow(1, "0/1 up")

	// a running project shows the success green dot; a fully-down one muted gray
	if !strings.Contains(rows, "38;2;63;185;80") {
		tt.Errorf("UP project should be success green (63;185;80):\n%s", rows)
	}
	if !strings.Contains(rows, "38;2;139;147;158") {
		tt.Errorf("DOWN project should be muted gray (139;147;158):\n%s", rows)
	}
}

func TestComposeListExpandableRows(tt *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	tt.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	m := New(nil)
	m.width = 150
	m.height = 30
	m.ready = true
	m.loading = false
	m.activeTab = tabCompose
	m.compose = []docker.ComposeProject{
		{Name: "webtier", Services: []docker.ComposeService{
			{Name: "api", Running: true},
			{Name: "db", Running: true},
			{Name: "web", Running: false},
		}, Running: 2, Total: 3, ConfigFiles: "docker-compose.yml"},
		{Name: "emptyproj", Services: []docker.ComposeService{{Name: "worker", Running: false}}, Running: 0, Total: 1, ConfigFiles: ""},
	}
	m.fitMainViewport()

	w, vw, h := innerW(m.width), innerW(m.width)-1, m.height-tabBarHeight-helpBarHeight
	raw := func() string {
		_, rows := m.renderComposeList(w, vw, h)
		return rows
	}
	lines := func() []string {
		return strings.Split(stripANSI(raw()), "\n")
	}

	if got := m.composeListRows(); got != 2 {
		tt.Fatalf("collapsed composeListRows = %d, want 2", got)
	}
	if l := lines(); len(l) != 2 {
		tt.Fatalf("collapsed render has %d rows, want 2", len(l))
	}

	// space on the webtier project row expands it below the project row.
	m.selectedIdx = 0
	m.toggleComposeExpanded()
	if got := m.composeListRows(); got != 5 {
		tt.Fatalf("expanded composeListRows = %d, want 5", got)
	}
	l := lines()
	if len(l) != 5 {
		tt.Fatalf("expanded render has %d rows, want %d:\n%s", len(l), 5, strings.Join(l, "\n"))
	}
	// the expanded project flips its chevron to ▼; collapsed ones stay ▶.
	if r := []rune(l[0]); string(r[1]) != "▼" {
		tt.Errorf("expanded project chevron = %q, want ▼\n%s", string(r[1]), l[0])
	}
	if r := []rune(l[4]); string(r[1]) != "▶" {
		tt.Errorf("collapsed project chevron = %q, want ▶\n%s", string(r[1]), l[4])
	}
	// project rows have no status dot: the name starts right after the
	// chevron+gap ("▸ " -> column 4). Service rows keep their dot at column 1
	// and tuck the branch/name under the parent with no extra indent.
	if got := string([]rune(l[0])[4:11]); got != "webtier" {
		tt.Errorf("expanded project should start its name at col 4: %q", got)
	}
	if r := []rune(l[1]); string(r[1]) != "●" || !strings.Contains(l[1], "api") || !strings.Contains(l[1], "├") {
		tt.Errorf("service row 1 should keep its dot and branch near the column 1: %q", l[1])
	}
	if !strings.Contains(l[2], "db") || !strings.Contains(l[2], "up") {
		tt.Errorf("service row 2 should show db up: %q", l[2])
	}
	if !strings.Contains(l[3], "web") || !strings.Contains(l[3], "down") {
		tt.Errorf("service row 3 should show web down: %q", l[3])
	}
	if !strings.Contains(l[4], "emptyproj") {
		tt.Errorf("row 4 should be the next project: %q", l[4])
	}

	// green + muted dots both present across service rows (checked on the raw
	// render, before ANSI stripping).
	rawRows := raw()
	if !strings.Contains(rawRows, "38;2;63;185;80") || !strings.Contains(rawRows, "38;2;139;147;158") {
		tt.Errorf("service rows must color up/down dots (green/muted):\n%s", stripANSI(rawRows))
	}

	// flat navigation: at the last project row, down clamps on the flat length
	m.selectedIdx = 4
	m.moveDown()
	if m.selectedIdx != 4 {
		tt.Errorf("moveDown at flat bottom = %d, want 4", m.selectedIdx)
	}
	m.selectedIdx = 3
	m.moveDown()
	if m.selectedIdx != 4 {
		tt.Errorf("moveDown from service row = %d, want 4", m.selectedIdx)
	}

	// space collapses the project again (from its own project row).
	m.selectedIdx = 0
	m.toggleComposeExpanded()
	if got := m.composeListRows(); got != 2 {
		tt.Fatalf("re-collapsed composeListRows = %d, want 2", got)
	}
	// selection was inside the collapsed project: it clamps back to its row.
	if m.selectedIdx != 0 {
		tt.Errorf("selection after collapse = %d, want 0", m.selectedIdx)
	}
}

func TestComposeServiceRowOpensMenu(tt *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.activeTab = tabCompose
	m.compose = []docker.ComposeProject{
		{Name: "webtier", Services: []docker.ComposeService{
			{Name: "api", Running: true},
			{Name: "worker", Running: false},
		}, Running: 1, Total: 2, ConfigFiles: "docker-compose.yml"},
	}
	m.composeExpanded["webtier"] = true
	x := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}

	m.selectedIdx = 1 // running service row
	next, cmd := testUpdate(m, x)
	if !next.menuOpen {
		tt.Fatal("x on a running service row should open the service menu")
	}
	if cmd != nil {
		tt.Error("x must not dispatch an action")
	}
	if got := next.menu.header; got != "Actions for service api" {
		tt.Errorf("service menu title = %q", got)
	}
	var labels, keys []string
	for _, it := range next.menu.items {
		labels = append(labels, it.label)
		keys = append(keys, it.key)
	}
	if want := []string{"Stop", "Restart", "Exec shell", "Logs"}; !reflect.DeepEqual(labels, want) {
		tt.Errorf("running service items = %q, want %q", labels, want)
	}
	if want := []string{"s", "r", "e", "l"}; !reflect.DeepEqual(keys, want) {
		tt.Errorf("running service keys = %q, want %q", keys, want)
	}
	if got := next.menu.dividers; !reflect.DeepEqual(got, []int{3}) {
		tt.Errorf("dividers = %v, want [3]", got)
	}
	for i, it := range next.menu.items {
		want := map[string]string{
			"Stop":       "docker compose stop api",
			"Restart":    "docker compose restart api",
			"Exec shell": "docker compose exec api sh",
			"Logs":       "docker compose logs -f api",
		}[it.label]
		if i < 2 && it.cli != want {
			tt.Errorf("item %d CLI = %q, want %q", i, it.cli, want)
		}
	}

	m.selectedIdx = 2 // down service row
	next, _ = testUpdate(m, x)
	if !next.menuOpen {
		tt.Fatal("x on a down service row should open the service menu")
	}
	if got := next.menu.header; got != "Actions for service worker" {
		tt.Errorf("down service menu title = %q", got)
	}
	labels, keys = nil, nil
	for _, it := range next.menu.items {
		labels = append(labels, it.label)
		keys = append(keys, it.key)
	}
	if want := []string{"Start", "Restart", "Logs"}; !reflect.DeepEqual(labels, want) {
		tt.Errorf("down service items = %q, want %q", labels, want)
	}
	if want := []string{"s", "r", "l"}; !reflect.DeepEqual(keys, want) {
		tt.Errorf("down service keys = %q, want %q", keys, want)
	}

	// the project row itself still opens the project menu
	m.selectedIdx = 0
	next, _ = testUpdate(m, x)
	if !next.menuOpen || next.menu.header != "Actions for project webtier" {
		tt.Errorf("x on the project row should open the project menu, got header = %q open=%v", next.menu.header, next.menuOpen)
	}
	if got := next.menu.items[0].label; got != "Up -d" {
		tt.Errorf("project menu first item = %q, want Up -d", got)
	}
}

func TestKeyXOpensNetworkMenu(tt *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.networks = []docker.Network{
		{Name: "bridge", Driver: "bridge", Scope: "local", Containers: 2},
		{Name: "orphan_net", Driver: "bridge", Scope: "local", Containers: 0},
	}
	m.activeTab = tabNetworks
	m.selectedIdx = 1
	m.fitViewports()

	x := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}
	next, cmd := testUpdate(m, x)
	if !next.menuOpen {
		tt.Fatal("x should open the popup on the selected network")
	}
	if cmd != nil {
		tt.Error("x must not dispatch an action")
	}
	if got := next.menu.header; got != "Actions for network orphan_net" {
		tt.Errorf("menu title = %q", got)
	}
	if got := next.menu.items[0].label; got != "Remove" {
		tt.Errorf("first item = %q, want Remove", got)
	}
	if want := max((next.width-next.menu.w)/2, 0); next.menu.x != want {
		tt.Errorf("popup x = %d, want centered %d", next.menu.x, want)
	}
}

func TestNetworkMenuAlwaysHasRemove(tt *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.activeTab = tabNetworks
	m.networks = []docker.Network{
		{Name: "db_net", Driver: "bridge", Scope: "local", Containers: 3},
		{Name: "orphan_net", Driver: "bridge", Scope: "local", Containers: 0},
	}
	m.fitViewports()

	// Unlike images/volumes there is no force variant, so the action is a
	// plain Remove for in-use networks too — the daemon refuses those and the
	// error surfaces as the toast.
	for _, c := range m.networks {
		items, _ := m.networkMenuItems(c)
		if got := items[0].label; got != "Remove" {
			tt.Errorf("network %q item = %q, want Remove", c.Name, got)
		}
		if got := items[0].cli; got != "docker network rm "+c.Name {
			tt.Errorf("network %q CLI = %q, want docker network rm %s", c.Name, got, c.Name)
		}
		if got := items[0].confirmHeader; got != "Remove "+c.Name+"?" {
			tt.Errorf("network %q confirm header = %q", c.Name, got)
		}
		if !items[0].confirm {
			tt.Errorf("network %q Remove must stage a confirm", c.Name)
		}
	}
}

func TestNetworkMenuPruneOnlyWithUnused(tt *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.activeTab = tabNetworks

	// all networks in use: only the single-network Remove is offered
	m.networks = []docker.Network{
		{Name: "db_net", Driver: "bridge", Containers: 1},
		{Name: "cache_net", Driver: "bridge", Containers: 2},
	}
	m.fitViewports()
	labelled, _ := m.networkMenuItems(m.networks[0])
	if len(labelled) != 1 {
		tt.Fatalf("menu without unused = %d items, want 1", len(labelled))
	}

	// one unused network unlocks the prune action
	m.networks = append(m.networks, docker.Network{Name: "orphan_net", Driver: "bridge", Containers: 0})
	m.fitViewports()
	items, dividers := m.networkMenuItems(m.networks[0])
	if len(items) != 2 {
		tt.Fatalf("menu with unused = %d items, want 2", len(items))
	}
	if got := items[1].label; got != "Prune unused" {
		tt.Errorf("second item = %q, want Prune unused", got)
	}
	if got := items[1].cli; got != "docker network prune" {
		tt.Errorf("prune CLI = %q, want docker network prune", got)
	}
	if !items[1].confirm {
		tt.Error("Prune unused must stage a confirm")
	}
	if got := items[1].confirmHeader; got != "Prune all unused networks?" {
		tt.Errorf("prune confirm header = %q", got)
	}
	// the bulk action is visually separated from the single-network Remove
	if len(dividers) != 1 || dividers[0] != 1 {
		tt.Errorf("dividers = %v, want [1] (before the prune item)", dividers)
	}
}

func TestNetworkMenuConfirmFlow(tt *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 30
	m.ready = true
	m.activeTab = tabNetworks
	m.networks = []docker.Network{
		{Name: "bridge", Driver: "bridge", Containers: 0},
	}
	m.selectedIdx = 0
	m.fitViewports()

	m = testMouseUpdate(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	enter := tea.KeyMsg{Type: tea.KeyEnter}
	next := testMouseUpdate(m, enter)
	if !next.menu.confirm {
		tt.Fatal("Enter on Remove should stage the confirm stage")
	}
	if got := next.menu.items[0].label; got != "Yes, remove" {
		tt.Errorf("confirm first item = %q, want Yes, remove", got)
	}
	if got := next.menu.header; got != "Remove bridge?" {
		tt.Errorf("confirm header = %q", got)
	}
}

func TestMenuPauseResume(tt *testing.T) {
	enter := tea.KeyMsg{Type: tea.KeyEnter}
	down := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")}

	// running container: Stop + Pause come before the Remove variants
	m := detailTestModel()
	m.containers = makeTestContainers(3)
	m.fitViewports()
	open := openMenuFor(m, 0)
	for i, want := range []string{"Stop", "Pause", "Exec shell", "Attach", "Restart", "Remove"} {
		if got := open.menu.items[i].label; got != want {
			tt.Errorf("running item %d = %q, want %q", i, got, want)
		}
	}
	// Pause closes the popup and dispatches the pause command
	m = open
	next, cmd := testUpdate(m, down)
	next, cmd = testUpdate(next, enter)
	if next.menuOpen {
		tt.Fatal("Enter on Pause should close the popup")
	}
	if cmd == nil {
		tt.Error("Pause must dispatch a command")
	}

	// paused container: only Resume for the state action
	pz := detailTestModel()
	c := makeTestContainers(1)[0]
	c.State = "paused"
	pz.containers = []docker.Container{c}
	pz.fitViewports()
	pz = openMenuFor(pz, 0)
	if got := pz.menu.items[0].label; got != "Resume" {
		tt.Fatalf("paused first item = %q, want Resume", got)
	}
	next, cmd = testUpdate(pz, enter)
	if next.menuOpen {
		tt.Fatal("Enter on Resume should close the popup")
	}
	if cmd == nil {
		tt.Error("Resume must dispatch a command")
	}
	// a paused container has no Pause item
	for _, it := range pz.menu.items {
		if it.label == "Pause" {
			tt.Error("paused container must not offer Pause")
		}
	}

	// non-running, non-paused: Start toggle only, no Pause
	m = detailTestModel()
	m.containers = makeTestContainers(3)
	m.fitViewports()
	ex := openMenuFor(m, 1) // exited
	if got := ex.menu.items[0].label; got != "Start" {
		tt.Errorf("exited first item = %q, want Start", got)
	}
	for _, it := range ex.menu.items {
		if it.label == "Pause" {
			tt.Error("exited container must not offer Pause")
		}
	}
}

func TestMenuPruneStoppedGating(tt *testing.T) {
	// a list that only holds running containers never offers the bulk prune
	allRunning := []docker.Container{{ID: "aaaaaaaaaaaa", Names: []string{"/web"}, Image: "img:latest", State: "running"}}
	m := detailTestModel()
	m.containers = allRunning
	m.fitViewports()
	items, dividers := m.containerMenuItems(allRunning[0])
	for _, it := range items {
		if it.label == "Prune stopped" {
			tt.Error("running-only list must not offer Prune stopped")
		}
	}
	if len(dividers) != 0 {
		tt.Errorf("running-only list dividers = %v, want none", dividers)
	}
	if m.hasStoppedContainers() {
		tt.Error("hasStoppedContainers should be false with only running containers")
	}

	// any stopped (exited) container in the list adds the separated prune item
	m.containers = append(m.containers, docker.Container{ID: "bbbbbbbbbbbb", Names: []string{"/worker"}, Image: "img:latest", State: "exited"})
	m.fitViewports()
	if !m.hasStoppedContainers() {
		tt.Error("hasStoppedContainers should be true once a stopped container exists")
	}
	items, dividers = m.containerMenuItems(m.containers[0])
	last := items[len(items)-1]
	if last.label != "Prune stopped" {
		tt.Errorf("last item = %q, want Prune stopped", last.label)
	}
	if !last.confirm {
		tt.Error("Prune stopped must stage a confirm")
	}
	if len(dividers) != 1 || dividers[0] != len(items)-1 {
		tt.Errorf("dividers = %v, want [%d] (before the prune item)", dividers, len(items)-1)
	}

	// running/paused/restarting containers are not prunable
	m.containers = []docker.Container{
		{ID: "aaaaaaaaaaaa", Names: []string{"/p"}, Image: "img:latest", State: "paused"},
		{ID: "bbbbbbbbbbbb", Names: []string{"/r"}, Image: "img:latest", State: "restarting"},
		{ID: "cccccccccccc", Names: []string{"/c"}, Image: "img:latest", State: "created"},
	}
	m.fitViewports()
	_, dividers = m.containerMenuItems(m.containers[0])
	if len(dividers) != 1 {
		tt.Errorf("only 'created' is stopped, dividers = %v, want one", dividers)
	}
}

func TestMenuPruneStoppedConfirmFlow(tt *testing.T) {
	m := detailTestModel()
	m.containers = makeTestContainers(3) // includes an exited container
	m.fitViewports()
	m.selectedIdx = 0
	m.menu = m.buildContainerMenu()
	m.menuOpen = true

	// the prune item is gated behind a divider at the bottom
	pruneIdx := len(m.menu.items) - 1
	if got := m.menu.items[pruneIdx].label; got != "Prune stopped" {
		tt.Fatalf("last item = %q, want Prune stopped", got)
	}

	// Enter on Prune stopped stages the daemon-wide confirm header
	next := testMouseUpdate(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	if !next.menu.confirm {
		tt.Fatal("pressing g on Prune stopped should stage the confirm")
	}
	if want := "Prune all stopped containers?"; next.menu.header != want {
		tt.Errorf("confirm header = %q, want %q", next.menu.header, want)
	}
	if got := next.menu.items[0].label; got != "Yes, remove" {
		tt.Errorf("confirm first item = %q, want Yes, remove", got)
	}
	// confirm stage clears the divider layout
	if len(next.menu.dividers) != 0 {
		tt.Errorf("confirm stage must clear dividers, got %v", next.menu.dividers)
	}
	// Esc cancels back to nothing (confirm stage has no stack)
	next = testMouseUpdate(next, tea.KeyMsg{Type: tea.KeyEsc})
	if next.menuOpen {
		tt.Fatal("Esc on the confirm stage should close the popup")
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
	for _, item := range tabBarItems() {
		if !strings.Contains(off, item) {
			tt.Errorf("tab bar is missing tab %q", item)
		}
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

func TestInsertStyledLinePreservesTailColors(tt *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	tt.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	border := lipgloss.NewStyle().
		Background(t.Background).Foreground(t.Border).
		Render(strings.Repeat("─", 40))

	popup := MenuBoxStyle.Render(strings.Repeat("x", 10))
	out := insertStyledLine(border, 10, 10, popup)

	// The popup lands first, its own style must be on the boxed cells.
	if !strings.Contains(out, string(popup)) {
		tt.Fatal("popup not present in spliced line")
	}

	// The dash run spans the splice: cells right of the block keep the border
	// foreground (#30363d = 48;54;60), not the app foreground (#e6edf3 =
	// 230;237;243) that the pre-fix repaint forced, turning the line white.
	tail := out[strings.Index(out, string(popup))+len(popup):]
	if !strings.Contains(tail, "38;2;48;54;60") {
		tt.Errorf("tail dash run not restored with border fg: %q", tail)
	}
	if strings.Contains(tail, "38;2;230;237;243") {
		tt.Errorf("tail dash run repainted with app fg: %q", tail)
	}

	// Visible layout: 10 dashes, the 10-cell popup, 20 trailing dashes.
	if got, want := stripANSI(out), strings.Repeat("─", 10)+strings.Repeat("x", 10)+strings.Repeat("─", 20); got != want {
		tt.Errorf("spliced line = %q, want %q", got, want)
	}
}

func TestInsertStyledLineKeepsMenuRightBorderMuted(tt *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	tt.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	m := detailTestModel()
	m.containers = makeTestContainers(3)
	m.fitViewports()
	m = openMenuFor(m, 0)
	if !m.menuOpen {
		tt.Fatal("menu not open")
	}

	// The right border cell of every popup row must be painted muted-on-surface.
	// The pre-fix item rows ended with the item style's reset, leaking the
	// trailing │ in the terminal default color.
	for r, row := range m.renderContainerMenu() {
		vis := []rune(stripANSI(row))
		if len(vis) == 0 {
			continue
		}
		right := vis[len(vis)-1]
		if !strings.ContainsRune("┌┐└┘│", right) {
			tt.Errorf("menu row %d does not end with a box glyph: %q", r, string(vis))
			continue
		}
		sgr := sgrStyleAt(row, right)
		if !strings.Contains(sgr, "139;147;158") {
			tt.Errorf("menu row %d right border %q not painted muted (style %q): %q", r, string(right), sgr, row)
		}
	}
}

// sgrStyleAt returns the SGR sequence styling the last occurrence of g in row
// (the sequence that begins at or before the glyph and governs its color).
func sgrStyleAt(row string, g rune) string {
	idx := strings.LastIndex(row, string(g))
	if idx < 0 {
		return ""
	}
	// walk back to the escape sequence that styles this cell
	j := idx
	for j > 0 && row[j] != '\x1b' {
		j--
	}
	k := j
	for k < len(row) && !((row[k] >= 'a' && row[k] <= 'z') || (row[k] >= 'A' && row[k] <= 'Z')) {
		k++
	}
	if k < len(row) {
		return row[j : k+1]
	}
	return ""
}

func TestParseSGRState(tt *testing.T) {
	cases := []struct {
		seq    string
		wantFG string
		wantBG string
	}{
		{"\x1b[38;2;48;54;60;48;2;13;17;23m", "38;2;48;54;60", "48;2;13;17;23"},
		{"\x1b[0m", "", ""},
		{"\x1b[39m", "", ""},
		{"\x1b[49m", "", ""},
		{"\x1b[1m", "", ""},
		{"\x1b[?25l", "", ""},
		{"\x1b[38;2;230;237;243;48;2;22;27;34m", "38;2;230;237;243", "48;2;22;27;34"},
	}
	for _, c := range cases {
		fg, bg := "", ""
		parseSGRState(c.seq, &fg, &bg)
		if fg != c.wantFG || bg != c.wantBG {
			tt.Errorf("parseSGRState(%q) = fg %q bg %q, want fg %q bg %q",
				c.seq, fg, bg, c.wantFG, c.wantBG)
		}
	}
	if got := sgrRestore("38;2;48;54;60", "48;2;13;17;23"); got != "\x1b[38;2;48;54;60;48;2;13;17;23m" {
		tt.Errorf("sgrRestore fg+bg = %q", got)
	}
	if got := sgrRestore("", ""); got != "" {
		tt.Errorf("sgrRestore default = %q, want empty", got)
	}
	if got := sgrRestore("38;2;48;54;60", ""); got != "\x1b[38;2;48;54;60m" {
		tt.Errorf("sgrRestore fg only = %q", got)
	}
}

// ----- floating terminal (docker exec / attach) -----

func TestTermScreenEditing(t *testing.T) {
	s := newTermScreen(40, 6, nil)
	s.Feed([]byte("hello\r\n"))
	if got := strings.TrimRight(s.Text()[0], " "); got != "hello" {
		t.Errorf("CRLF line = %q, want hello", got)
	}
	if x, y := s.t.CursorX(), s.t.CursorY(); x != 0 || y != 1 {
		t.Errorf("cursor after CRLF = %d,%d, want 0,1", x, y)
	}
	s.Feed([]byte("a\x1b[31mb\x1b[0mc"))
	if got := strings.TrimRight(s.Text()[1], " "); got != "abc" {
		t.Errorf("SGR-split line = %q, want abc", got)
	}
	if c := termCellAt(s, 1, 1); !c.fgSet || c.fg != termPalette[1] {
		t.Errorf("SGR cell fg missing: %+v", c)
	}
	if c := termCellAt(s, 2, 1); c.fgSet {
		t.Errorf("reset cell still painted: %+v", c)
	}
	// an OSC payload is skipped wholesale
	s.Feed([]byte("\r\nx\x1b]0;title\x07y\n"))
	if got := strings.TrimRight(s.Text()[2], " "); got != "xy" {
		t.Errorf("OSC-stripped line = %q, want xy", got)
	}
	// tab jumps to the next 8-column stop
	s.Feed([]byte("ab\t"))
	if s.t.CursorX() != 8 {
		t.Errorf("tab stop cursor = %d, want 8", s.t.CursorX())
	}
	// BS (0x08) moves the cursor left; the raw line editor of the shell drives
	// the actual erase and sends this sequence after editing
	s.Feed([]byte("\x08"))
	if s.t.CursorX() != 7 {
		t.Errorf("backspace cursor = %d, want 7", s.t.CursorX())
	}
	// multibyte runs land intact
	s.Feed([]byte("\r\nГо!"))
	if got := strings.TrimRight(s.Text()[4], " "); got != "Го!" {
		t.Errorf("multibyte line = %q", got)
	}
}

func TestTermScreenCursorAndErase(t *testing.T) {
	s := newTermScreen(40, 6, nil)
	s.Feed([]byte("abc\x1b[D"))
	if s.t.CursorX() != 2 {
		t.Errorf("cursor left = %d, want 2", s.t.CursorX())
	}
	s.Feed([]byte("Z"))
	if got := strings.TrimRight(s.Text()[0], " "); got != "abZ" {
		t.Errorf("typed-over line = %q, want abZ", got)
	}
	s.Feed([]byte("\x1b[2DQ"))
	if s.t.CursorX() != 2 {
		t.Errorf("cursor after moves = %d, want 2", s.t.CursorX())
	}
	if got := strings.TrimRight(s.Text()[0], " "); got != "aQZ" {
		t.Errorf("typed-over line = %q, want aQZ", got)
	}
	// erase-in-line clears the whole row that contains the cursor
	s.Feed([]byte("\x1b[2K"))
	if got := strings.TrimRight(s.Text()[0], " "); got != "" {
		t.Errorf("erase-in-line left %q", got)
	}
	// an editable blank line follows CUP
	s.Feed([]byte("\x1b[5;1H"))
	if x, y := s.t.CursorX(), s.t.CursorY(); x != 0 || y != 4 {
		t.Errorf("CUP cursor = %d,%d, want 0,4", x, y)
	}
	// DSR asks for the cursor position and the emulator answers
	var replies []string
	s2 := newTermScreen(40, 6, func(r string) { replies = append(replies, r) })
	s2.Feed([]byte("ab\x1b[6n"))
	if len(replies) != 1 || replies[0] != "\x1b[1;3R" {
		t.Errorf("DSR replies = %q, want \\x1b[1;3R", replies)
	}
	// DECTCEM hides and shows the block cursor
	s2.Feed([]byte("\x1b[?25l"))
	if !s2.t.IsCursorHidden() {
		t.Error("?25l must hide the cursor")
	}
	s2.Feed([]byte("\x1b[?25h"))
	if s2.t.IsCursorHidden() {
		t.Error("?25h must show the cursor")
	}
}

func TestTermScreenScrollAndWrap(t *testing.T) {
	s := newTermScreen(5, 3, nil)
	s.Feed([]byte("012345"))
	if got := strings.TrimRight(s.Text()[0], " "); got != "01234" {
		t.Errorf("wrapped first row = %q, want 01234", got)
	}
	if got := strings.TrimRight(s.Text()[1], " "); got != "5" {
		t.Errorf("wrapped second row = %q, want 5", got)
	}
	s2 := newTermScreen(40, 3, nil)
	s2.Feed([]byte("1\r\n2\r\n3\r\n4\r\n"))
	// every linefeed at the bottom row scrolls, so after four CRLF'd lines
	// the window shows the last three and the bottom row waits for the cursor
	if got := strings.TrimRight(s2.Text()[0], " "); got != "3" {
		t.Errorf("scrolled row0 = %q, want 3", got)
	}
	if got := strings.TrimRight(s2.Text()[1], " "); got != "4" {
		t.Errorf("scrolled row1 = %q, want 4", got)
	}
	if got := strings.TrimRight(s2.Text()[2], " "); got != "" {
		t.Errorf("bottom row = %q, want empty", got)
	}
}

func TestTermScreenScrollbackAndSnap(t *testing.T) {
	s := newTermScreen(20, 3, nil)
	for i := 1; i <= 9; i++ {
		s.Feed([]byte(fmt.Sprintf("L%d\r\n", i)))
	}
	s.Feed([]byte("> "))
	if s.scrolledUp() {
		t.Fatal("fresh terminal at bottom must not report scrolledUp")
	}
	if got := strings.TrimRight(s.Text()[0], " "); got != "L8" {
		t.Fatalf("bottom viewport row0 = %q, want L8", got)
	}
	// page back into history three lines
	s.scrollView(-3)
	if !s.scrolledUp() {
		t.Error("scrollView(-3) must enter scrollback")
	}
	if got := strings.TrimRight(s.Text()[0], " "); got != "L5" {
		t.Errorf("scrolled row0 = %q, want L5", got)
	}
	// scrolling past the top clamps instead of wrapping
	s.scrollView(-999)
	if got := strings.TrimRight(s.Text()[0], " "); got != "L1" {
		t.Errorf("clamped top row0 = %q, want L1", got)
	}
	// snap returns to the live cursor
	s.snapToBottom()
	if s.scrolledUp() {
		t.Error("snapToBottom must leave scrollback")
	}
	if got := strings.TrimRight(s.Text()[0], " "); got != "L8" {
		t.Errorf("snapped row0 = %q, want L8", got)
	}
	if got := strings.TrimRight(s.Text()[2], " "); got != ">" {
		t.Errorf("snapped cursor row = %q, want '>'", got)
	}
}

func TestTermForwardPgUpSnapsAndAltScreen(t *testing.T) {
	ptmx, slave, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open: %v", err)
	}
	defer ptmx.Close()
	defer slave.Close()
	rawSlave(slave)

	m := detailTestModel()
	term := &termFloat{ptmx: ptmx, args: []string{"exec", "-it", "web", "sh"}}
	term.x, term.y, term.w, term.h = m.termPanelLayout()
	term.emu = newTermScreen(max(term.w-2*termInset, 1), max(term.h-5, 1), nil)
	m.term = term
	for i := 1; i <= 30; i++ {
		term.emu.Feed([]byte(fmt.Sprintf("L%d\r\n", i)))
	}
	term.emu.Feed([]byte("> "))
	top := term.emu.Text()[0]

	readSlave := func() string {
		buf := make([]byte, 16)
		n, _ := slave.Read(buf)
		return string(buf[:n])
	}

	// PgUp pages the embedded history and must not leak bytes to the shell
	if _, cmd := testUpdate(m, tea.KeyMsg{Type: tea.KeyPgUp}); cmd != nil {
		t.Errorf("PgUp must not dispatch app commands, got %T", cmd)
	}
	if !term.emu.scrolledUp() {
		t.Error("PgUp must scroll the viewport into history")
	}

	// a plain keystroke snaps back to the live view and reaches the shell
	_, _ = testUpdate(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if term.emu.scrolledUp() {
		t.Error("plain key after PgUp must snap to the live view")
	}
	if got := term.emu.Text()[0]; got != top {
		t.Errorf("viewport after key snap = %q, want back at %q", got, top)
	}
	if s := readSlave(); s != "x" {
		t.Errorf("key reached shell as %q, want x", s)
	}

	// alternate-screen apps (vim) own PgUp: it must be forwarded untouched
	term.emu.Feed([]byte("\x1b[?1049h"))
	if !term.emu.isAlt() {
		t.Fatal("1049h must activate the alternate buffer")
	}
	testUpdate(m, tea.KeyMsg{Type: tea.KeyPgUp})
	if s := readSlave(); s != "\x1b[5~" {
		t.Errorf("alternate-screen PgUp = %q, want \\x1b[5~", s)
	}
}

func TestTermWheelScrollsViewport(t *testing.T) {
	ptmx, slave, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open: %v", err)
	}
	defer ptmx.Close()
	defer slave.Close()
	rawSlave(slave)

	m := detailTestModel()
	term := &termFloat{ptmx: ptmx, args: []string{"exec", "-it", "web", "sh"}}
	term.x, term.y, term.w, term.h = m.termPanelLayout()
	term.emu = newTermScreen(max(term.w-2*termInset, 1), max(term.h-5, 1), nil)
	m.term = term
	for i := 1; i <= 30; i++ {
		term.emu.Feed([]byte(fmt.Sprintf("L%d\r\n", i)))
	}
	term.emu.Feed([]byte("> "))

	// magic coords inside the console (bubbletea X/Y are 1-based): the console
	// starts at (term.x+termInset, term.y+4) below the header, its divider,
	// the top divider and the padding row.
	bodyX, bodyY := term.x+termInset+1, term.y+5
	if _, cmd := testUpdate(m, tea.MouseMsg{Type: tea.MouseWheelUp, Action: tea.MouseActionMotion, X: bodyX, Y: bodyY}); cmd != nil {
		t.Errorf("wheel must not dispatch app commands, got %T", cmd)
	}
	if !term.emu.scrolledUp() {
		t.Error("wheel up over the body must scroll into history")
	}
	if _, _ = testUpdate(m, tea.MouseMsg{Type: tea.MouseWheelDown, Action: tea.MouseActionMotion, X: bodyX, Y: bodyY}); term.emu.scrolledUp() {
		t.Error("wheel down over the body must scroll back to the live view")
	}
	// the header/divider/padding area swallows clicks without closing anything
	if _, cmd := testUpdate(m, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: term.x + termInset + 1, Y: term.y + 1}); cmd != nil {
		t.Errorf("header click must not dispatch commands, got %v", cmd)
	}
	if m2, _ := testUpdate(m, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: term.x + termInset + 1, Y: term.y + 1}); m2.term == nil {
		t.Error("header click must not tear the session down")
	}
}

func TestTermMouseForwarding(t *testing.T) {
	var replies []string
	s := newTermScreen(20, 4, func(r string) { replies = append(replies, r) })

	// with no mouse-tracking mode enabled nothing is emitted and callers can
	// fall back to scrollback behaviour
	if s.forwardMouse(3, 2, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress}) {
		t.Error("mouse forward must stay off without an app mouse mode")
	}
	if len(replies) != 0 {
		t.Fatalf("no mode yet but got reports %q", replies)
	}

	// SGR mouse tracking (1003 = report everything, 1006 = SGR encoding)
	s.Feed([]byte("\x1b[?1003h\x1b[?1006h"))
	if !s.forwardMouse(3, 2, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress}) {
		t.Fatal("mouse forward must engage with 1003+1006")
	}
	if want := "\x1b[<0;4;3M"; len(replies) == 0 || replies[len(replies)-1] != want {
		t.Errorf("SGR press report = %q, want %q", lastOr(replies), want)
	}

	s.forwardMouse(0, 0, tea.MouseMsg{Type: tea.MouseWheelDown, Action: tea.MouseActionMotion})
	if want := "\x1b[<65;1;1M"; replies[len(replies)-1] != want {
		t.Errorf("SGR wheel report = %q, want %q", replies[len(replies)-1], want)
	}

	// release reports use lowercase m in SGR
	s.forwardMouse(0, 0, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionRelease})
	if want := "\x1b[<0;1;1m"; replies[len(replies)-1] != want {
		t.Errorf("SGR release report = %q, want %q", replies[len(replies)-1], want)
	}
}

func lastOr(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1]
}

// TestTermColoredCellKeepsPanelSurface guards the frameless panel: cells with
// a foreground colour but no explicit background must render with the panel
// surface behind them, otherwise the frame underneath the floating terminal
// shows through as a patchy background.
func TestTermColoredCellKeepsPanelSurface(tt *testing.T) {
	// lipgloss downgrades to the Ascii profile when stdout is not a TTY;
	// force TrueColor so the escape codes the fix relies on are actually emitted.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	tt.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	surface := parseHexRGB(string(t.Surface))
	s := newTermScreen(20, 3, nil)
	s.Feed([]byte("\x1b[31m" + strings.Repeat("R", 20)))
	row := s.renderRow(0)
	want := fmt.Sprintf("48;2;%d;%d;%d", surface[0], surface[1], surface[2])
	if !strings.Contains(row, want) {
		tt.Fatalf("colored fg-only row must keep the panel surface bg, got %q", row)
	}
	if w := lipgloss.Width(row); w != 20 {
		tt.Fatalf("row width = %d, want 20", w)
	}
}

func parseHexRGB(hex string) [3]int {
	hex = strings.TrimPrefix(hex, "#")
	v, _ := strconv.ParseUint(hex, 16, 32)
	return [3]int{int(v >> 16 & 0xff), int(v >> 8 & 0xff), int(v & 0xff)}
}

func TestTermScreenSGR(t *testing.T) {
	s := newTermScreen(40, 6, nil)
	s.Feed([]byte("\x1b[38;5;196mR\x1b[38;2;1;2;3mT\x1b[0mX"))
	if c := termCellAt(s, 0, 0); !c.fgSet || c.fg != termPalette[196] {
		t.Errorf("256-color cell = %+v", c)
	}
	if c := termCellAt(s, 1, 0); !c.fgSet || c.fg != lipgloss.Color("#010203") {
		t.Errorf("truecolor cell = %+v", c)
	}
	if c := termCellAt(s, 2, 0); c.fgSet {
		t.Errorf("reset cell still painted: %+v", c)
	}
	s.Feed([]byte("\x1b[44mA\x1b[0m"))
	if c := termCellAt(s, 3, 0); !c.bgSet || c.bg != termPalette[4] {
		t.Errorf("background cell = %+v", c)
	}
	s.Feed([]byte("\x1b[1;4;7mB"))
	if c := termCellAt(s, 4, 0); !c.bold || !c.underline || !c.reverse {
		t.Errorf("attributes cell = %+v", c)
	}
}

// termCellAt reads one cell of the emulator screen for assertions.
func termCellAt(s *termScreen, x, y int) termCell {
	buf := s.buffer()
	if y < 0 || y >= s.t.Rows() {
		return termCell{}
	}
	line := buf.Lines.Get(buf.YDisp + y)
	if x < 0 || x >= s.t.Cols() || x >= line.Len {
		return termCell{}
	}
	cell := xterm.NewCellData()
	line.LoadCell(x, cell)
	return cellFromData(cell)
}

func TestTermRenderPanelAndSplice(tt *testing.T) {
	m := detailTestModel()
	term := &termFloat{args: []string{"exec", "-it", "web", "sh"}}
	term.x, term.y, term.w, term.h = m.termPanelLayout()
	term.h = 7
	term.emu = newTermScreen(max(term.w-2*termInset, 1), max(term.h-5, 1), nil)
	term.append([]byte("root@abc:/#\r\n"))
	m.term = term

	// lipgloss downgrades to the Ascii profile when stdout is not a TTY;
	// force TrueColor so the yellow header escape is actually emitted.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	tt.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	rows := m.renderTerminalPanel()
	if len(rows) != term.h {
		tt.Errorf("panel rows = %d, want %d", len(rows), term.h)
	}
	for _, r := range rows {
		if lipgloss.Width(r) != term.w {
			tt.Errorf("panel row width = %d, want %d: %q", lipgloss.Width(r), term.w, stripANSI(r))
		}
	}
	header := stripANSI(rows[1])
	if !strings.Contains(header, "docker exec -it web sh") {
		tt.Errorf("header = %q, want the docker invocation", header)
	}
	if !strings.HasPrefix(header, "  docker") {
		tt.Errorf("header = %q, want the termInset left padding before the title", header)
	}
	// the title is yellow on the surface background
	if !strings.Contains(rows[1], "38;2;210;153;34") {
		tt.Errorf("header must be yellow, got %q", rows[1])
	}
	if strings.Contains(header, "×") {
		tt.Error("header must have no close badge after the divider redesign")
	}
	if strings.ContainsAny(header, "│┌─└┘") {
		tt.Errorf("frameless header leaked border glyphs: %q", header)
	}
	// the header is boxed between two divider rows
	for i := 0; i < 3; i += 2 {
		if d := stripANSI(rows[i]); !strings.Contains(d, "─") {
			tt.Errorf("divider row %d = %q, want a ─ separator", i, d)
		}
	}
	// the console body is the padded middle region (two dividers, the header,
	// one padding row top, one padding row bottom)
	body := stripANSI(strings.Join(rows[4:term.h-1], "\n"))
	if !strings.Contains(body, "root@abc:/#") {
		tt.Errorf("body = %q, want the shell prompt", body)
	}

	// unterminated typed input shows on the cursor row, before Enter
	term.append([]byte("ls -la"))
	rows = m.renderTerminalPanel()
	lb := stripANSI(strings.Join(rows[4:term.h-1], "\n"))
	if !strings.Contains(lb, "ls -la") {
		tt.Errorf("body = %q, want the typed input visible", lb)
	}

	// the panel splices into the frame and every covered row keeps frame width
	frameLines := strings.Split(m.View(), "\n")
	if len(frameLines) != m.height {
		tt.Fatalf("frame rows = %d, want %d", len(frameLines), m.height)
	}
	for r := 0; r < term.h; r++ {
		row := frameLines[term.y+r]
		if lipgloss.Width(row) != m.width {
			tt.Errorf("frame row %d width = %d, want %d", term.y+r, lipgloss.Width(row), m.width)
		}
	}
	// the header text is visible within the spliced frame (row 0 is the top
	// divider, the header sits below it)
	if !strings.Contains(stripANSI(frameLines[term.y+1]), "docker exec -it web sh") {
		tt.Errorf("spliced header row = %q", stripANSI(frameLines[term.y+1]))
	}
}

func TestTermForwardAndStream(tt *testing.T) {
	ptmx, slave, err := pty.Open()
	if err != nil {
		tt.Fatalf("pty.Open: %v", err)
	}
	defer ptmx.Close()
	defer slave.Close()
	// The docker CLI would put the slave into raw mode; do the same here so
	// reads return per-key instead of waiting for a canonical newline.
	rawSlave(slave)

	m := detailTestModel()
	term := &termFloat{ptmx: ptmx, args: []string{"exec", "-it", "web", "sh"}}
	term.x, term.y, term.w, term.h = m.termPanelLayout()
	term.emu = newTermScreen(max(term.w-2*termInset, 1), max(term.h-5, 1), nil)
	m.term = term

	readSlave := func() string {
		got := make(chan string, 1)
		go func() {
			buf := make([]byte, 16)
			n, _ := slave.Read(buf)
			got <- string(buf[:n])
		}()
		select {
		case s := <-got:
			return s
		case <-time.After(time.Second):
			return ""
		}
	}

	// 'e' reaches the guest's stdin verbatim
	testUpdate(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if s := readSlave(); s != "e" {
		tt.Errorf("forward of 'e' reached guest as %q, want e", s)
	}

	// every key is swallowed while the float is open - q included - but the
	// byte still travels to the pty
	next, cmd := testUpdate(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd != nil {
		tt.Errorf("forwarded keystroke must not dispatch app commands, got %T", cmd)
	}
	if next.term == nil {
		tt.Fatal("forwarding must not close the float")
	}
	if s := readSlave(); s != "q" {
		tt.Errorf("forward of 'q' reached guest as %q, want q", s)
	}

	// special keys map to the terminal byte sequences a shell expects
	for _, c := range []struct {
		key  tea.KeyMsg
		want []byte
	}{
		{tea.KeyMsg{Type: tea.KeyBackspace}, []byte{0x7f}},
		{tea.KeyMsg{Type: tea.KeyEnter}, []byte{'\r'}},
		{tea.KeyMsg{Type: tea.KeyTab}, []byte{'\t'}},
		{tea.KeyMsg{Type: tea.KeyEsc}, []byte{'\x1b'}},
		{tea.KeyMsg{Type: tea.KeyUp}, []byte("\x1b[A")},
		{tea.KeyMsg{Type: tea.KeyCtrlC}, []byte{3}},
	} {
		testUpdate(next, c.key)
		if s := readSlave(); s != string(c.want) {
			tt.Errorf("%s => guest got %q, want %q", c.key.String(), s, string(c.want))
		}
	}

	// output streamed into the model lands on the emulated screen and
	// re-issues the reader
	next, cmd = testUpdate(next, termOutputMsg([]byte("hello\n")))
	if next.term.emu == nil || !strings.Contains(strings.Join(next.term.emu.Text(), "\n"), "hello") {
		tt.Errorf("term output emulator = %q, want hello on screen", next.term.emu.Text())
	}
	if cmd == nil {
		tt.Error("termOutputMsg must re-issue the reader command")
	}

	// a normal exit (EIO once the guest halves are gone) closes the float
	slave.Close()
	next, cmd = testUpdate(next, termExitMsg{err: syscall.EIO})
	if next.term != nil {
		tt.Error("termExitMsg must close the float")
	}
	if cmd == nil {
		tt.Error("termExitMsg must refresh the lists")
	}
}

// rawSlave switches a pty slave into raw mode so reads return single bytes
// immediately (canonical ICANON mode would buffer until a newline).
func rawSlave(f *os.File) {
	ti, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	if err != nil {
		panic(err)
	}
	ti.Iflag &^= unix.ICRNL | unix.IXON
	ti.Lflag &^= unix.ICANON | unix.ECHO | unix.ISIG
	if err := unix.IoctlSetTermios(int(f.Fd()), unix.TCSETS, ti); err != nil {
		panic(err)
	}
}

func TestTermStartReplacesSession(tt *testing.T) {
	ptmx1, slave1, err := pty.Open()
	if err != nil {
		tt.Fatalf("pty.Open #1: %v", err)
	}
	defer ptmx1.Close()
	defer slave1.Close()
	ptmx2, slave2, err := pty.Open()
	if err != nil {
		tt.Fatalf("pty.Open #2: %v", err)
	}
	defer ptmx2.Close()
	defer slave2.Close()

	m := detailTestModel()
	m.term = &termFloat{ptmx: ptmx1}
	next, cmd := testUpdate(m, termStartMsg{ptmx: ptmx2, args: []string{"attach", "--sig-proxy=false", "web"}})
	if next.term == nil || next.term.ptmx != ptmx2 {
		tt.Fatal("termStartMsg must install the new session on the new master")
	}
	if got := next.term.title(); got != "docker attach --sig-proxy=false web" {
		tt.Errorf("title = %q, want docker attach --sig-proxy=false web", got)
	}
	if next.term.emu == nil {
		tt.Error("termStartMsg must size the emulator to the panel body")
	}
	if cmd == nil {
		tt.Error("termStartMsg must start the reader")
	}
}

func TestMenuExecAttachItems(tt *testing.T) {
	items, _ := detailTestModel().containerMenuItems(makeTestContainers(1)[0])
	labels := make([]string, len(items))
	for i, it := range items {
		labels[i] = it.label
	}
	want := []string{"Stop", "Pause", "Exec shell", "Attach", "Restart", "Remove"}
	for i, w := range want {
		if i >= len(labels) || labels[i] != w {
			tt.Errorf("running menu %v, want %v", labels, want)
			break
		}
	}
	var execCLI, attachCLI string
	for _, it := range items {
		switch it.label {
		case "Exec shell":
			execCLI = it.cli
		case "Attach":
			attachCLI = it.cli
		}
	}
	if execCLI != "docker exec -it test-container-0 sh" {
		tt.Errorf("exec cli = %q", execCLI)
	}
	if attachCLI != "docker attach --sig-proxy=false test-container-0" {
		tt.Errorf("attach cli = %q", attachCLI)
	}
}

func TestExecAttachGating(tt *testing.T) {
	m := detailTestModel()
	m.containers = makeTestContainers(3)
	m.selectedIdx = 0 // running
	if m.execShell() == nil {
		tt.Error("exec on a running container must dispatch")
	}
	if m.attachContainer() == nil {
		tt.Error("attach on a running container must dispatch")
	}
	m.selectedIdx = 1 // exited
	if m.execShell() != nil {
		tt.Error("exec must be gated on a running container")
	}
	if m.attachContainer() != nil {
		tt.Error("attach must be gated on a running container")
	}
	m.selectedIdx = 0
	m.activeTab = tabImages
	if m.execShell() != nil {
		tt.Error("exec must be gated on the containers tab")
	}
	if m.attachContainer() != nil {
		tt.Error("attach must be gated on the containers tab")
	}

	// hotkeys: e/t on the containers tab start a session for a running pick
	m = detailTestModel()
	m.selectedIdx = 0
	m, _ = testUpdate(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if m.term != nil {
		tt.Fatal("pressing e must not open the float synchronously (cmd starts it)")
	}
}
