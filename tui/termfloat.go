package tui

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/creack/pty"
	"github.com/kuri4/dockerherocker/docker"
)

// termFloat is a running docker exec / attach session embedded in a floating
// panel inside the UI. The docker CLI runs on its own pseudo-tty: its raw
// output is rendered (ANSI-stripped, shell line-discipline aware) into the
// panel body and every keystroke is forwarded into the pty, so the container
// shell behaves like a real interactive terminal. The session ends when the
// shell exits (exit/ctrl+d) or detaches (ctrl-p ctrl-q inside docker attach);
// the × badge in the panel header kills it too.
type termFloat struct {
	ptmx *os.File
	cmd  *exec.Cmd
	args []string // docker CLI arguments (the header shows "docker " + args)

	x, y int // panel top-left cell in the final view grid
	w, h int // panel box size in cells

	body    []string // rendered lines, newest last
	esc     termEsc  // ANSI escape parser state
	curLine []byte   // current line being assembled
}

// termEsc tracks whether output bytes are inside an ANSI escape sequence.
type termEsc int

const (
	escNone termEsc = iota
	escCSI          // skipping until a final byte (ESC[...  / ESC O...)
	escOSC          // skipping until BEL (ESC]...<BEL>)
)

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

// execShell opens an interactive sh inside the selected running container in a
// floating terminal panel.
func (m Model) execShell() tea.Cmd {
	c, ok := m.selectedRunningContainer()
	if !ok {
		return nil
	}
	return m.launchTerminal("exec", "-it", containerDisplayName(c), "sh")
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

// append feeds raw pty output through the line-discipline processor.
func (t *termFloat) append(data []byte) {
	for _, b := range data {
		t.write(b)
	}
	t.clampBody()
}

// write processes one output byte, emulating enough terminal behaviour to
// render an interactive shell: ANSI escapes are skipped, \b\b/0x7f undo the
// last cell, \r and stray control characters are dropped and lines are split
// on \n.
func (t *termFloat) write(b byte) {
	switch t.esc {
	case escOSC:
		if b == '\x07' {
			t.esc = escNone
		}
		return
	case escCSI:
		switch b {
		case ']':
			t.esc = escOSC
		default:
			if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') {
				t.esc = escNone
			}
		}
		return
	}
	switch {
	case b == '\x1b':
		t.esc = escCSI
	case b == '\n':
		t.pushLine()
	case b == '\b' || b == '\x7f':
		t.backspace()
	case b == '\t':
		t.curLine = append(t.curLine, ' ', ' ', ' ', ' ')
	case b == '\r' || (b < 0x20):
		// drops carriage returns (shells emit \r for screen rewrites) and any
		// other control byte (bell, etc.)
	default:
		t.curLine = append(t.curLine, b)
	}
}

// pushLine moves the assembled line into the body.
func (t *termFloat) pushLine() {
	t.body = append(t.body, string(t.curLine))
	t.curLine = t.curLine[:0]
	t.clampBody()
}

// backspace removes the last rendered cell, undoing the whole trailing UTF-8
// rune when the pty echo rewrote multibyte output.
func (t *termFloat) backspace() {
	if len(t.curLine) == 0 {
		if n := len(t.body); n > 0 {
			t.body = t.body[:n-1]
		}
		return
	}
	last := t.curLine[len(t.curLine)-1]
	t.curLine = t.curLine[:len(t.curLine)-1]
	for len(t.curLine) > 0 && last >= 0x80 && last <= 0xBF {
		last = t.curLine[len(t.curLine)-1]
		t.curLine = t.curLine[:len(t.curLine)-1]
	}
}

// clampBody keeps only the last h-4 lines (the box has top border, header,
// rule and bottom border around the body) so the panel never grows unbounded.
func (t *termFloat) clampBody() {
	if cap := max(t.h-4, 1); len(t.body) > cap {
		t.body = t.body[len(t.body)-cap:]
	}
}

// title is the header text, normalised to a docker CLI invocation.
func (t *termFloat) title() string {
	return "docker " + strings.Join(t.args, " ")
}

// forwardToTerm sends a keystroke into the pty and swallows it: while the
// float is open every key (including q/ctrl+c) belongs to the terminal.
func (m Model) forwardToTerm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.term == nil || m.term.ptmx == nil {
		return m, nil
	}
	if b := termKeyBytes(msg); len(b) > 0 {
		_, _ = m.term.ptmx.Write(b)
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
	case tea.KeyEsc:
		return []byte{'\x1b'}
	case tea.KeyDelete:
		return []byte("\x1b[3~")
	case tea.KeyInsert:
		return []byte("\x1b[2~")
	case tea.KeyHome:
		return []byte("\x1b[H")
	case tea.KeyEnd:
		return []byte("\x1b[F")
	case tea.KeyPgUp:
		return []byte("\x1b[5~")
	case tea.KeyPgDown:
		return []byte("\x1b[6~")
	case tea.KeyUp:
		return []byte("\x1b[A")
	case tea.KeyDown:
		return []byte("\x1b[B")
	case tea.KeyRight:
		return []byte("\x1b[C")
	case tea.KeyLeft:
		return []byte("\x1b[D")
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

// renderTerminalPanel builds the floating panel rows (header + rule + body +
// borders) of exactly w cells each, in the same style family as the context
// menu popup.
func (m Model) renderTerminalPanel() []string {
	t := m.term
	if t == nil {
		return nil
	}
	pw := t.w
	iw := max(pw-2, 0)
	box := MenuBoxStyle

	rows := []string{box.Render("┌" + strings.Repeat("─", iw) + "┐")}

	// Header: the docker invocation (orange) with a click-to-close × badge
	// right-aligned at the inner edge.
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
		var line string
		if r < len(t.body) {
			line = padMenuRunes(t.body[r], iw)
		} else {
			line = strings.Repeat(" ", iw)
		}
		rows = append(rows, box.Render("│")+MenuItemStyle.Render(line)+box.Render("│"))
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
