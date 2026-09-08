package tui

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/creack/pty"
)

// Run with: RUN_LIVE=1 go test ./tui/ -run TestLiveUIFrame -count=1 -v
//
// Drives the REAL app input path: bubbletea KeyMsg -> forwardToTerm -> pty ->
// container shell -> xterm emulator -> m.View(). Frames are dumped so broken
// arrow/tab handling is visible exactly as the user sees it.
func TestLiveUIFrame(t *testing.T) {
	if os.Getenv("RUN_LIVE") == "" {
		t.Skip("set RUN_LIVE=1 for the live docker exec check")
	}
	which := os.Getenv("LIVE_SHELL")
	if which == "" {
		which = "busybox"
	}
	container := "tui_bash"
	switch which {
	case "busybox":
		container = "tui_live"
	case "postgres":
		container = "t1_test-postgres-1"
	}

	shell := "sh"
	if which != "bare-sh" {
		shell = detectInteractiveShell(container)
	}
	cmd := exec.Command("docker", "exec", "-it", container, shell)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 6, Cols: 40})
	if err != nil {
		t.Fatalf("docker exec: %v", err)
	}
	defer ptmx.Close()

	reply := func(p string) { _, _ = ptmx.Write([]byte(p)) }
	m := detailTestModel()
	term := &termFloat{ptmx: ptmx, args: []string{"exec", "-it", container, shell}}
	term.x, term.y, term.w, term.h = m.termPanelLayout()
	term.h = 6
	term.emu = newTermScreen(max(term.w-2*termInset, 1), max(term.h-4, 1), reply)
	m.term = term

	// Reader goroutine: pty -> emulator (mirrors termOutputMsg path).
	stop := make(chan struct{})
	go func() {
		buf := make([]byte, 8192)
		for {
			select {
			case <-stop:
				return
			default:
			}
			n, err := ptmx.Read(buf)
			if n > 0 {
				term.append(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	defer close(stop)

	wait := func(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }
	sendKey := func(k tea.KeyMsg) {
		updated, _ := m.Update(k)
		m = updated.(Model)
	}
	frame := func() {
		body := m.View()
		lines := strings.Split(body, "\n")
		for _, ln := range lines {
			if strings.Contains(ln, "docker") || strings.Contains(ln, "├") || strings.Contains(ln, "│") {
				fmt.Printf("    %s\n", stripANSI(ln))
			}
		}
	}

	t.Logf("=== shell=%s ===", which)
	wait(400)
	frame()

	t.Logf("--- type 'ls' ---")
	sendKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	sendKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	wait(250)
	frame()

	t.Logf("--- arrows Left x2 then 'X' ---")
	sendKey(tea.KeyMsg{Type: tea.KeyLeft})
	sendKey(tea.KeyMsg{Type: tea.KeyLeft})
	wait(150)
	frame()
	sendKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("X")})
	wait(150)
	frame()

	t.Logf("--- backspace x2 ---")
	sendKey(tea.KeyMsg{Type: tea.KeyBackspace})
	sendKey(tea.KeyMsg{Type: tea.KeyBackspace})
	wait(150)
	frame()

	t.Logf("--- fresh prompt, tab completion: 'cd /bo<Tab>' ---")
	sendKey(tea.KeyMsg{Type: tea.KeyEnter})
	wait(200)
	frame()
	for _, r := range "cd /bo" {
		sendKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	wait(120)
	sendKey(tea.KeyMsg{Type: tea.KeyTab})
	wait(250)
	frame()
}
