package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	xterm "github.com/gitpod-io/xterm-go"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/creack/pty"
	"github.com/kuri4/dockerherocker/docker"
)

// termFloat is a running docker exec / attach session embedded in a floating
// panel inside the UI. The docker CLI runs on its own pseudo-tty (raw at both
// ends, so the container shell owns line editing); the pty output is decoded
// by an embedded xterm (github.com/gitpod-io/xterm-go, a headless port of the
// xterm.js state machine) and repainted in the panel with the block cursor and
// SGR colours, while every keystroke is forwarded verbatim into the pty. The
// session ends when the shell exits (exit/ctrl+d) or detaches (ctrl-p ctrl-q
// inside docker attach); the × badge in the panel header kills it too.
type termFloat struct {
	ptmx *os.File
	cmd  *exec.Cmd
	args []string // docker CLI arguments (the header shows "docker " + args)

	x, y int // panel top-left cell in the final view grid
	w, h int // panel box size in cells

	emu *termScreen // pty output emulator, sized to the panel body
}

// termStartMsg carries a freshly spawned docker exec/attach session.
type termStartMsg struct {
	ptmx *os.File
	cmd  *exec.Cmd
	args []string
}

// termOutputMsg is a chunk of raw output read from the terminal's pty master.
type termOutputMsg []byte

// termExitMsg reports that the child session ended (EOF/EIO on the pty or a
// real error).
type termExitMsg struct{ err error }

// selectedRunningContainer returns the selected container when the Containers
// tab is active and the container is running (the state docker exec/attach
// require).
func (m Model) selectedRunningContainer() (docker.Container, bool) {
	c, ok := m.selectedContainer()
	if !ok || c.State != "running" {
		return docker.Container{}, false
	}
	return c, true
}

// execShell opens an interactive shell inside the selected running container.
// The concrete shell is probed first (bash / ash when present) and falls back
// to bare sh: dash and other minimal POSIX shells have no line editing, so the
// container kernel would echo raw escape sequences ("^[[D") and tab completion
// would not exist — the user would see garbage instead of a working console.
func (m Model) execShell() tea.Cmd {
	c, ok := m.selectedRunningContainer()
	if !ok {
		return nil
	}
	name := containerDisplayName(c)
	return m.launchTerminal("exec", "-it", name, detectInteractiveShell(name))
}

// detectInteractiveShell probes the container for a shell with real line
// editing (readline), preferring bash over busybox ash, before falling back to
// the bare POSIX sh.
func detectInteractiveShell(container string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "exec", container, "sh", "-c",
		"command -v bash || command -v ash || command -v zsh || echo sh").Output()
	if err != nil {
		return "sh"
	}
	if s := strings.TrimSpace(string(out)); s != "" {
		return s
	}
	return "sh"
}

// attachContainer attaches to the console of the selected running container.
// Ctrl-P Ctrl-Q detaches back to the app; with --sig-proxy=false ctrl+c
// aborts the attach session instead of signalling the container's process.
// (docker attach enables stdin by default, so no -i is needed.)
func (m Model) attachContainer() tea.Cmd {
	c, ok := m.selectedRunningContainer()
	if !ok {
		return nil
	}
	return m.launchTerminal("attach", "--sig-proxy=false", containerDisplayName(c))
}

// launchTerminal spawns a docker CLI session on its own pty sized to the
// floating panel, so the container shell is genuinely interactive. Failures
// (docker missing from PATH, daemon refusing) surface through the errMsg
// toast; anything the CLI prints will be captured by the panel instead.
func (m Model) launchTerminal(args ...string) tea.Cmd {
	argcopy := append([]string{}, args...)
	if _, err := exec.LookPath("docker"); err != nil {
		return func() tea.Msg { return errMsg{err} }
	}
	cmd := exec.Command("docker", args...)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	_, _, w, h := m.termPanelLayout()
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Rows: uint16(max(h-4, 1)),
		Cols: uint16(max(w-2, 1)),
	})
	if err != nil {
		return func() tea.Msg { return errMsg{err} }
	}
	return func() tea.Msg { return termStartMsg{ptmx: ptmx, cmd: cmd, args: argcopy} }
}

// termPanelLayout computes the floating panel geometry: slightly inset from
// the terminal, centered, avoiding the tab bar.
func (m Model) termPanelLayout() (x, y, w, h int) {
	inner := innerW(m.width)
	w = max(20, inner-4)
	if w > inner {
		w = inner
	}
	h = max(8, m.height-6)
	x = max(1, (m.width-w)/2)
	y = max(tabBarHeight+2, (m.height-h)/2)
	return x, y, w, h
}

// termReader returns a Cmd that reads the next chunk from the pty master. It
// is re-issued after every chunk so the session streams until EOF/EIO.
func (m Model) termReader() tea.Cmd {
	t := m.term
	if t == nil || t.ptmx == nil {
		return nil
	}
	return func() tea.Msg {
		buf := make([]byte, 8192)
		n, err := t.ptmx.Read(buf)
		if err != nil {
			return termExitMsg{err: err}
		}
		out := make([]byte, n)
		copy(out, buf[:n])
		return termOutputMsg(out)
	}
}

// resize re-sizes the emulator and the pty to the current panel geometry.
// The child shell redraws after the SIGWINCH the pty resize delivers.
func (t *termFloat) resize() {
	if t.emu != nil {
		t.emu.resize(max(t.w-2, 1), max(t.h-4, 1))
	}
}

// append feeds raw pty output into the terminal emulator.
func (t *termFloat) append(data []byte) {
	if t.emu != nil {
		t.emu.Feed(data)
	}
}

// title is the header text, normalised to a docker CLI invocation.
func (t *termFloat) title() string {
	return "docker " + strings.Join(t.args, " ")
}

// forwardToTerm sends a keystroke into the pty and swallows it: while the
// float is open every key (including q/ctrl+c) belongs to the terminal. On the
// normal screen PgUp/PgDn page the embedded scrollback instead of the shell;
// in alternate-screen apps (vim, ...) they are forwarded like any other key.
// A plain keystroke after scrolling back snaps the view to the live output.
func (m Model) forwardToTerm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	t := m.term
	if t == nil || t.ptmx == nil {
		return m, nil
	}
	if !t.emu.isAlt() {
		switch msg.Type {
		case tea.KeyPgUp:
			t.emu.scrollView(-max(t.emu.rows(), 1))
			return m, nil
		case tea.KeyPgDown:
			t.emu.scrollView(max(t.emu.rows(), 1))
			return m, nil
		}
	}
	t.emu.snapToBottom()
	if b := termKeyBytes(msg); len(b) > 0 {
		_, _ = t.ptmx.Write(b)
	}
	return m, nil
}

// termKeyBytes maps a tea keypress to the terminal byte sequence the shell
// expects (including ANSI escapes for navigation keys).
func termKeyBytes(msg tea.KeyMsg) []byte {
	if msg.Type == tea.KeyRunes && len(msg.Runes) > 0 {
		return []byte(string(msg.Runes))
	}
	switch msg.Type {
	case tea.KeyEnter:
		return []byte{'\r'}
	case tea.KeyBackspace:
		return []byte{'\x7f'}
	case tea.KeyTab:
		return []byte{'\t'}
	case tea.KeyShiftTab:
		return []byte("\x1b[Z")
	case tea.KeySpace:
		return []byte{' '}
	case tea.KeyUp:
		return []byte("\x1b[A")
	case tea.KeyDown:
		return []byte("\x1b[B")
	case tea.KeyRight:
		return []byte("\x1b[C")
	case tea.KeyLeft:
		return []byte("\x1b[D")
	case tea.KeyHome:
		return []byte("\x1b[H")
	case tea.KeyEnd:
		return []byte("\x1b[F")
	case tea.KeyPgUp:
		return []byte("\x1b[5~")
	case tea.KeyPgDown:
		return []byte("\x1b[6~")
	case tea.KeyDelete:
		return []byte("\x1b[3~")
	case tea.KeyInsert:
		return []byte("\x1b[2~")
	case tea.KeyEsc:
		return []byte{'\x1b'}
	case tea.KeyCtrlUp:
		return []byte("\x1b[1;5A")
	case tea.KeyCtrlDown:
		return []byte("\x1b[1;5B")
	case tea.KeyCtrlRight:
		return []byte("\x1b[1;5C")
	case tea.KeyCtrlLeft:
		return []byte("\x1b[1;5D")
	}
	if msg.Type >= tea.KeyCtrlA && msg.Type <= tea.KeyCtrlZ {
		return []byte{byte(msg.Type - tea.KeyCtrlA + 1)}
	}
	return nil
}

// ----- the embedded terminal -----

// termScreen wraps github.com/gitpod-io/xterm-go, a headless port of the
// reference xterm.js state machine, and adapts it to the floating panel: pty
// output is fed into the terminal, the visible viewport is read back as a cell
// grid, and the block cursor / SGR colours are painted with the same lipgloss
// styles as the rest of the TUI. Host-bound requests (the DSR cursor position
// report, DA, ...) are answered automatically through the reply callback back
// into the pty. All state lives on the bubbletea goroutine - Feed happens
// inside Update, rendering inside View - so no locking is needed.
type termScreen struct {
	t     *xterm.Terminal
	reply func(string)
}

// newTermScreen creates a terminal sized to the panel body. reply receives the
// bytes the terminal generates for the host (DSR/DA responses) and is wired
// back into the pty by the caller.
func newTermScreen(cols, rows int, reply func(string)) *termScreen {
	s := &termScreen{
		t:     xterm.New(xterm.WithCols(max(cols, 1)), xterm.WithRows(max(rows, 1))),
		reply: reply,
	}
	if reply != nil {
		s.t.OnData(func(data string) { reply(data) })
	}
	return s
}

// resize re-sizes the terminal viewport.
func (s *termScreen) resize(cols, rows int) {
	s.t.Resize(max(cols, 1), max(rows, 1))
}

// rows returns the emulator's viewport height.
func (s *termScreen) rows() int { return s.t.Rows() }

// cols returns the emulator's viewport width.
func (s *termScreen) cols() int { return s.t.Cols() }

// Feed decodes raw pty output.
func (s *termScreen) Feed(data []byte) {
	_, _ = s.t.Write(data)
}

// buffer is the active screen buffer (normal or alternate, whatever the child
// program switched to).
func (s *termScreen) buffer() *xterm.Buffer { return s.t.Buffer() }

// scrolledUp reports whether the viewport has been scrolled back into history.
func (s *termScreen) scrolledUp() bool {
	b := s.buffer()
	return b.HasScrollback() && b.YDisp < b.YBase
}

// snapToBottom returns the viewport to the live output. Real terminals do this
// on any keystroke after the user scrolled back (unless the key is a scroll
// key, which forwardToTerm already filtered out).
func (s *termScreen) snapToBottom() {
	if s.scrolledUp() {
		s.t.ScrollToBottom()
	}
}

// scrollView moves the viewport by delta lines (negative = back into history).
// ScrollLines clamps at the top of scrollback and the cursor row, so this is
// safe to spam.
func (s *termScreen) scrollView(delta int) {
	s.t.ScrollLines(delta)
}

// isAlt reports whether the child program is on the alternate screen buffer
// (vim, htop, a full-screen TUI), where scrollback is meaningless and PgUp /
// the wheel belong to the program, not to history paging.
func (s *termScreen) isAlt() bool { return s.t.IsAltBufferActive() }

// forwardMouse hands a panel mouse event to the child program. The emit only
// happens when the program enabled a mouse-tracking mode (vim's drag, text
// selection in less, htop, ...); otherwise it returns false so the caller can
// fall back to scrollback behaviour.
func (s *termScreen) forwardMouse(col, row int, ev tea.MouseMsg) bool {
	btn, action := coreMouseFor(ev)
	if btn < 0 {
		return false
	}
	return s.t.TriggerMouseEvent(xterm.CoreMouseEvent{
		Col:    col + 1,
		Row:    row + 1,
		X:      col,
		Y:      row,
		Button: btn,
		Action: action,
		Ctrl:   ev.Ctrl,
		Alt:    ev.Alt,
		Shift:  ev.Shift,
	})
}

// coreMouseFor maps a bubbletea mouse message onto the xterm mouse protocol.
// It returns btn == -1 for events that must never be forwarded (a motion over
// an unpressed button, i.e. not a drag).
func coreMouseFor(ev tea.MouseMsg) (xterm.CoreMouseButton, xterm.CoreMouseAction) {
	action := xterm.MouseActionDown
	switch ev.Action {
	case tea.MouseActionRelease:
		action = xterm.MouseActionUp
	case tea.MouseActionMotion:
		action = xterm.MouseActionMove
	}
	switch ev.Type {
	case tea.MouseLeft:
		return xterm.MouseButtonLeft, action
	case tea.MouseRight:
		return xterm.MouseButtonRight, action
	case tea.MouseMiddle:
		return xterm.MouseButtonMiddle, action
	case tea.MouseWheelUp:
		return xterm.MouseButtonWheel, xterm.MouseActionUp
	case tea.MouseWheelDown:
		return xterm.MouseButtonWheel, xterm.MouseActionDown
	}
	return -1, 0
}

// Text returns the visible viewport as plain full-width strings, used by tests.
func (s *termScreen) Text() []string {
	cols := s.t.Cols()
	buf := s.buffer()
	out := make([]string, s.t.Rows())
	cell := xterm.NewCellData()
	for y := 0; y < len(out); y++ {
		line := buf.Lines.Get(buf.YDisp + y)
		var b strings.Builder
		for x := 0; x < cols; x++ {
			if x < line.Len {
				line.LoadCell(x, cell)
			} else {
				cell = xterm.NewCellData()
			}
			if cell.GetWidth() == 0 {
				continue
			}
			if ch := cell.GetChars(); ch != "" {
				b.WriteString(ch)
			} else {
				b.WriteByte(' ')
			}
		}
		out[y] = b.String()
	}
	return out
}

// termCell is one visible grid cell.
type termCell struct {
	ch   rune // displayed rune, 0 = blank
	w    int  // display columns (2 for wide runes)
	cont bool // continuation half of a wide rune

	fg, bg                   lipgloss.Color
	fgSet, bgSet             bool
	bold, underline, reverse bool
}

// termPalette maps the ANSI palette indices the terminal reports to RGB hexes:
// 0-15 are the classic xterm colours, 16-231 the 6x6x6 colour cube, 232-255 the
// grey ramp.
var termPalette = func() []lipgloss.Color {
	p := make([]lipgloss.Color, 256)
	base := [16][3]uint8{
		{0x00, 0x00, 0x00}, {0xcd, 0x00, 0x00}, {0x00, 0xcd, 0x00}, {0xcd, 0xcd, 0x00},
		{0x00, 0x00, 0xee}, {0xcd, 0x00, 0xcd}, {0x00, 0xcd, 0xcd}, {0xe5, 0xe5, 0xe5},
		{0x7f, 0x7f, 0x7f}, {0xff, 0x00, 0x00}, {0x00, 0xff, 0x00}, {0xff, 0xff, 0x00},
		{0x5c, 0x5c, 0xff}, {0xff, 0x00, 0xff}, {0x00, 0xff, 0xff}, {0xff, 0xff, 0xff},
	}
	for i := 0; i < 16; i++ {
		p[i] = rgbColor(base[i][0], base[i][1], base[i][2])
	}
	for i := 16; i < 232; i++ {
		v := i - 16
		cub := func(c int) uint8 { return uint8(55 + c*40) }
		p[i] = rgbColor(cub(v/36), cub(v/6%6), cub(v%6))
	}
	for i := 232; i < 256; i++ {
		g := uint8(8 + (i-232)*10)
		p[i] = rgbColor(g, g, g)
	}
	return p
}()

func rgbColor(r, g, b uint8) lipgloss.Color {
	return lipgloss.Color(fmt.Sprintf("#%02x%02x%02x", r, g, b))
}

// cellFromData converts one terminal cell + its SGR paint into a termCell.
func cellFromData(cell *xterm.CellData) termCell {
	tc := termCell{w: cell.GetWidth()}
	if tc.w == 0 {
		tc.cont = true
		return tc
	}
	if ch := cell.GetChars(); ch != "" {
		tc.ch = rune(cell.GetCode())
		tc.w = max(tc.w, 1)
	}
	if !cell.IsFgDefault() {
		if cell.IsFgRGB() {
			v := xterm.ToColorRGB(uint32(cell.GetFgColor()))
			tc.fg, tc.fgSet = rgbColor(v[0], v[1], v[2]), true
		} else if cell.IsFgPalette() {
			if idx := int(cell.GetFgColor()); idx >= 0 && idx < len(termPalette) {
				tc.fg, tc.fgSet = termPalette[idx], true
			}
		}
	}
	if !cell.IsBgDefault() {
		if cell.IsBgRGB() {
			v := xterm.ToColorRGB(uint32(cell.GetBgColor()))
			tc.bg, tc.bgSet = rgbColor(v[0], v[1], v[2]), true
		} else if cell.IsBgPalette() {
			if idx := int(cell.GetBgColor()); idx >= 0 && idx < len(termPalette) {
				tc.bg, tc.bgSet = termPalette[idx], true
			}
		}
	}
	tc.bold = cell.IsBold() != 0
	tc.underline = cell.IsUnderline() != 0
	tc.reverse = cell.IsInverse() != 0
	return tc
}

// paintGroup is a run of adjacent cells sharing the same SGR paint.
type paintGroup struct {
	text   string
	cell   termCell // paint of the run
	cursor bool     // the block cursor sits on this run's first cell
}

// renderRow paints one visible viewport row (0 = top) and draws the block
// cursor as a reverse-video cell. The returned string is exactly cols display
// cells wide.
func (s *termScreen) renderRow(y int) string {
	cols := s.t.Cols()
	if y < 0 || y >= s.t.Rows() {
		return MenuItemStyle.Render(strings.Repeat(" ", cols))
	}
	buf := s.buffer()
	line := buf.Lines.Get(buf.YDisp + y)
	cell := xterm.NewCellData()

	ctx, curY := s.t.CursorX(), s.t.CursorY()
	cursorOn := !s.t.IsCursorHidden() && buf.IsCursorInViewport()

	var groups []paintGroup
	g := paintGroup{cell: termCell{}}
	flush := func() {
		if g.text != "" {
			groups = append(groups, g)
		}
		g = paintGroup{}
	}
	samePaint := func(a, b termCell) bool {
		return a.fgSet == b.fgSet && a.bgSet == b.bgSet &&
			a.bold == b.bold && a.underline == b.underline && a.reverse == b.reverse &&
			a.fg == b.fg && a.bg == b.bg
	}
	for x := 0; x < cols; {
		var c termCell
		if x < line.Len {
			line.LoadCell(x, cell)
			c = cellFromData(cell)
		}
		if c.cont {
			x++
			continue
		}
		w := c.w
		if w < 1 {
			w = 1
		}
		cursor := cursorOn && y == curY && x == ctx
		if g.text != "" && (!samePaint(g.cell, c) || cursor) {
			flush()
		}
		if g.text == "" {
			g.cell = c
			g.cursor = cursor
		}
		if c.ch != 0 {
			g.text += string(c.ch)
		} else {
			g.text += " "
		}
		if cursor {
			// the block cursor is exactly one cell: close the group so the
			// reverse video does not spill across the rest of the row
			flush()
		}
		x += w
	}
	flush()
	if len(groups) == 0 {
		groups = []paintGroup{{text: strings.Repeat(" ", cols)}}
	}
	var out strings.Builder
	for _, gr := range groups {
		out.WriteString(gr.render())
	}
	return out.String()
}

// render styles one paint run: unstyled text uses the panel surface so the
// default row background survives embedded colours.
func (g paintGroup) render() string {
	c := g.cell
	if !c.fgSet && !c.bgSet && !c.bold && !c.underline && g.cursor != c.reverse {
		st := MenuItemStyle
		if g.cursor {
			st = st.Copy().Reverse(true)
		}
		return st.Render(g.text)
	}
	return lipgloss.NewStyle().
		Foreground(c.fg).
		Background(c.bg).
		Bold(c.bold).
		Underline(c.underline).
		Reverse(g.cursor || c.reverse).
		Render(g.text)
}

// renderBody paints the panel body row r (0 = top), the emulator's screen.
func (t *termFloat) renderBody(r int) string {
	if t.emu == nil {
		return MenuItemStyle.Render(strings.Repeat(" ", max(t.w-2, 0)))
	}
	return t.emu.renderRow(r)
}

// ----- the floating panel -----

// renderTerminalPanel draws the box, header (the docker invocation with a
// click-to-close × badge) and the emulator screen as the body.
func (m Model) renderTerminalPanel() []string {
	t := m.term
	if t == nil {
		return nil
	}
	pw := t.w
	iw := max(pw-2, 0)
	box := MenuBoxStyle

	rows := []string{box.Render("┌" + strings.Repeat("─", iw) + "┐")}

	titleW := max(iw-4, 0)
	title := padMenuRunes(t.title(), titleW)
	rows = append(rows,
		box.Render("│")+
			box.Render(" ")+
			MenuTitleStyle.Render(title)+
			box.Render(" ")+
			MenuCliStyle.Render("×")+
			box.Render(" ")+
			box.Render("│"),
	)
	rows = append(rows, box.Render("│"+strings.Repeat("─", iw)+"│"))

	for r := 0; r < max(t.h-4, 1); r++ {
		rows = append(rows, box.Render("│")+t.renderBody(r)+box.Render("│"))
	}
	rows = append(rows, box.Render("└"+strings.Repeat("─", iw)+"┘"))
	return rows
}

// spliceTerminal overlays the floating panel onto the fully rendered frame,
// exactly like splicePopup does for the context menu.
func (m Model) spliceTerminal(content string) string {
	if m.term == nil {
		return content
	}
	lines := strings.Split(content, "\n")
	for r, pr := range m.renderTerminalPanel() {
		sy := m.term.y + r
		if sy < 0 || sy >= len(lines) {
			continue
		}
		lines[sy] = insertStyledLine(lines[sy], m.term.x, m.term.w, pr)
	}
	return strings.Join(lines, "\n")
}

// closeTerminal tears the session down: it closes the pty (unblocking the
// reader) and, with a short grace period, kills the docker CLI if it did not
// exit on its own.
func (m *Model) closeTerminal() {
	t := m.term
	if t == nil {
		return
	}
	if t.ptmx != nil {
		_ = t.ptmx.Close()
	}
	if t.cmd != nil && t.cmd.Process != nil {
		done := make(chan struct{})
		go func() {
			_ = t.cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(300 * time.Millisecond):
			_ = t.cmd.Process.Kill()
			select {
			case <-done:
			case <-time.After(300 * time.Millisecond):
			}
		}
	}
	m.term = nil
}

// termExitedSilently reports whether the pty read failed because the session
// ended normally (EOF or the Linux EIO returned after the shell closed).
func termExitedSilently(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, syscall.EIO)
}
