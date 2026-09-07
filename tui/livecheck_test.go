package tui

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// Run with: RUN_LIVE=1 go test ./tui/ -run TestLiveXterm -count=1 -v
// Requires a running container "tui_live" (busybox) to exec into, mirroring
// the app's exec-shell path against the real xterm-go wrapper.
func TestLiveXterm(t *testing.T) {
	if os.Getenv("RUN_LIVE") == "" {
		t.Skip("set RUN_LIVE=1 for the live docker exec check")
	}
	cmd := exec.Command("docker", "exec", "-it", "tui_live", "sh")
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 6, Cols: 40})
	if err != nil {
		t.Fatalf("docker exec: %v", err)
	}
	defer ptmx.Close()

	var reply []byte
	emu := newTermScreen(38, 2, func(p string) { reply = append(reply, []byte(p)...) })

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
				emu.Feed(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	defer close(stop)

	fail := ""
	try := func(name string, ok bool, got string) {
		t.Logf("%-32s ok=%v%s", name, ok, func() string {
			if ok {
				return ""
			}
			return "  got=" + got
		}())
		if !ok && fail == "" {
			fail = name
		}
	}

	wait := func(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }
	send := func(s string) {
		if _, err := ptmx.Write([]byte(s)); err != nil {
			t.Fatalf("write %q: %v", s, err)
		}
	}
	rows := func() string { return strings.Join(emu.Text(), "|") }

	// A fresh prompt; busybox asks for the cursor position with DSR and xterm
	// answers through the reply callback back into the pty.
	send("\r")
	wait(500)
	try("prompt visible", strings.Contains(rows(), "#"), rows())
	// DSR auto-reply (busybox / alpine sh sends \x1b[6n at the prompt, xterm
	// answers \x1b[y;xR through OnData -> reply -> ptmx) is covered by the
	// unit test TestTermScreenCursorAndErase. Not all busybox builds emit it.

	// Echo pipeline: typed command + its output land on the emulated screen.
	r0x := emu.t.CursorX()
	send("printf abc\r")
	wait(400)
	try("command echo", strings.Contains(rows(), "printf abc"), rows())
	try("command output", strings.Contains(rows(), "abc"), rows())
	_ = r0x

	// Arrow keys reach the shell and the emulator's cursor follows the shell's
	// \x08 backspacing (busybox edit mode).
	send("echo xy")
	wait(300)
	try("typed line", strings.Contains(rows(), "echo xy"), rows())
	before := emu.t.CursorX()
	send("\x1b[D\x1b[D") // two left arrows
	wait(300)
	after := emu.t.CursorX()
	try("arrows move cursor left", after == before-2, rows())

	if fail != "" {
		t.Errorf("live check failed at: %s", fail)
	}
}
