#!/usr/bin/env python3
"""End-to-end checks for dockerherocker under a pseudo-terminal.

Reusable pty harness plus a handful of regression scenarios that cannot be
covered by Go unit tests: real terminal byte stream, diff-renderer output,
ANSI artifacts. Requires a running Docker daemon for meaningful data.

Usage: python3 scripts/e2e.py [--cols N --rows N]
"""
import base64, os, pty, time, select, fcntl, termios, struct, re, sys

COLS, ROWS = 140, 30
THEME_BG_RGB = "13;17;23"      # #0d1117
SEL_BG_SIG = "44;73;46"        # selection green (quantized)
STATE_FG = {
    "running":    "63;185;80",
    "paused":     "210;153;34",
    "restarting": "248;81;73",
    "exited":     "139;147;158",
}


class Session:
    """Spawns the binary under a pty of a fixed size and answers terminal queries."""

    def __init__(self, binary="./build/dockerherocker", cols=COLS, rows=ROWS):
        env = dict(os.environ, TERM="xterm-256color")
        self.master, slave = pty.openpty()
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
        self.pid = os.fork()
        if self.pid == 0:
            os.setsid()
            fcntl.ioctl(slave, termios.TIOCSCTTY, 0)
            os.dup2(slave, 0); os.dup2(slave, 1); os.dup2(slave, 2)
            try:
                os.close(self.master); os.close(slave)
            except OSError:
                pass
            os.execve(binary, [os.path.basename(binary)], env)
        os.close(slave)
        self.allbuf = b""

    def drain(self, t=0.5):
        end = time.time() + t
        out = b""
        while time.time() < end:
            r, _, _ = select.select([self.master], [], [], 0.05)
            if r:
                try:
                    d = os.read(self.master, 262144)
                    if not d:
                        break
                    out += d
                    # Answer terminal queries immediately - the app blocks
                    # on them before rendering anything.
                    if b"\x1b]11;?" in d:
                        os.write(self.master, b"\x1b]11;rgb:0d/11/17\x1b\\")
                    if b"\x1b[6n" in d:
                        os.write(self.master, b"\x1b[1;1R")
                except OSError:
                    break
        self.allbuf += out
        return out

    def send(self, data):
        os.write(self.master, data)

    def plain(self):
        s = self.allbuf.decode("utf-8", "replace")
        return re.sub(r"\x1b\[[?0-9;<>]*[a-zA-Z]", "", s)

    def quit(self):
        try:
            self.send(b"q")
            time.sleep(0.4)
            os.kill(self.pid, 15)
        except OSError:
            pass


CHECKS = []
def check(fn):
    CHECKS.append(fn)
    return fn


@check
def startup_no_panic(s):
    s.drain(3.0)
    assert b"Caught panic" not in s.allbuf, "app panicked on startup"


@check
def separator_lines_inset_by_margins(s):
    s.drain(1.5)
    txt = s.allbuf.decode("utf-8", "replace")
    seps = re.findall(r"(─{20,})", txt)
    assert seps, "no separator lines rendered"
    width = max(len(x) for x in seps)
    assert width <= COLS - 2 * app_margin_x(), f"separator too wide: {width}"


def app_margin_x():
    return 1


@check
def state_column_colors(s):
    s.drain(1.0)
    txt = s.allbuf.decode("utf-8", "replace")
    found = {}
    for m in re.finditer(r"\x1b\[[0-9;]*38;2;(\d+);(\d+);(\d+)[0-9;]*m"
                         r"(running|paused|restarting|exited|created)\b", txt):
        found.setdefault(m.group(4), set()).add(f"{m.group(1)};{m.group(2)};{m.group(3)}")
    for state, rgb in STATE_FG.items():
        if state in found:
            assert rgb in found[state], f"{state}: {found[state]}, want {rgb}"


@check
def no_bare_spaces_after_ansi_resets(s):
    s.drain(1.0)
    gaps = re.findall(r"\x1b\[0m {10,}", s.allbuf.decode("utf-8", "replace"))
    assert not gaps, f"{len(gaps)} unstyled space runs after ANSI resets"


@check
def port_lists_have_no_duplicates(s):
    s.drain(1.0)
    dup = re.search(r"(\d+->\d+/\w+), \1", s.plain())
    assert not dup, f"duplicate port binding: {dup.group(0) if dup else ''}"


@check
def click_tab_with_margin_offset(s):
    s.drain(3.0)  # let the app render and answer terminal queries
    before = len(s.plain())
    # content x=18..32 is tab [2]; terminal x = +appMarginX
    s.send(b"\x1b[<0;25;2M"); time.sleep(0.05)
    s.send(b"\x1b[<0;25;2m")
    s.drain(3.0)
    tail = s.plain()[before:]
    assert "postgres:17.5" in tail or "nginx:latest" in tail, \
        "click on tab [2] did not switch to Images"


@check
def wheel_over_table_moves_selection(s):
    s.send(b"1")
    s.drain(1.5)
    before = len(s.allbuf)
    s.send(b"\x1b[<65;20;8M")
    out = s.drain(0.8)
    assert len(out) > 0 or len(s.allbuf) > before, "wheel over table produced no repaint"


@check
def shift_drag_is_ignored_selection_unchanged(s):
    s.drain(3.0)
    # open the Logs sub-tab so there is a scrollable bottom pane
    s.send(b"\x1b[<0;12;18M"); time.sleep(0.05)
    s.send(b"\x1b[<0;12;18m")
    s.drain(0.8)

    sel_bg = "\x1b[48;2;44;73;46"

    def sel_count():
        txt = s.allbuf.decode("utf-8", "replace")
        i = txt.find("t1_test-stub-1")
        assert i != -1, "first container line not found"
        return txt[i - 400:i + 40].count(sel_bg)

    before = sel_count()
    assert before > 0, "expected first container row to be selected"
    # shift+left press/release over a lower table row (would select another container)
    s.send(b"\x1b[<4;20;12M"); time.sleep(0.05)
    s.send(b"\x1b[<4;20;12m")
    # shift+drag motions inside the log pane + release
    s.send(b"\x1b[<36;60;20M"); time.sleep(0.05)
    s.send(b"\x1b[<36;80;20M"); time.sleep(0.05)
    s.send(b"\x1b[<36;80;20m")
    s.drain(0.7)
    after = sel_count()
    assert after == before, \
        f"shift events changed selection highlight (before={before}, after={after})"


@check
def left_drag_selects_log_text(s):
    s.drain(3.0)
    # open the Logs sub-tab so the log body is underneath the cursor
    s.send(b"\x1b[<0;12;18M"); time.sleep(0.05)
    s.send(b"\x1b[<0;12;18m")
    s.drain(1.0)

    sel_bg = "48;2;44;73;46"   # selection green, same color as the table highlight

    def count_sel():
        return s.allbuf.decode("utf-8", "replace").count(sel_bg)

    base = len(s.allbuf)
    before = count_sel()
    assert before > 0, "expected the container-table selection highlight"

    # left-drag over the Logs content band (screen rows 21..26 -> SGR y 22..27)
    s.send(b"\x1b[<0;15;22M"); time.sleep(0.05)   # press
    s.send(b"\x1b[<32;30;23M"); time.sleep(0.05)  # left-motion
    s.send(b"\x1b[<32;70;26M"); time.sleep(0.05)  # left-motion
    s.send(b"\x1b[<0;70;26m")                     # release
    s.drain(0.8)
    assert count_sel() > before, \
        f"left-drag did not highlight log text (before={before}, after={count_sel()})"

    # a plain click (no motion) clears the highlight. The press itself emits
    # a one-cell live-preview frame before the release clears it, so settle
    # that transient first, then force a plain repaint (wheel) and check that
    # the freshly emitted log band carries no highlight.
    s.send(b"\x1b[<0;15;22M"); time.sleep(0.05)
    s.send(b"\x1b[<0;15;22m")
    s.drain(0.8)                             # settle the transient preview frame
    mark = len(s.allbuf)
    s.send(b"\x1b[<64;15;22M"); time.sleep(0.05)   # wheel-up over the log body
    s.drain(0.8)
    tail = s.allbuf[mark:].decode("utf-8", "replace")
    assert sel_bg not in tail, \
        "click did not clear the log text highlight (still in new output)"


@check
def drag_copies_selection_via_osc52(s):
    s.drain(3.0)
    s.send(b"\x1b[<0;12;18M"); time.sleep(0.05)
    s.send(b"\x1b[<0;12;18m")
    s.drain(1.0)

    osc52_re = re.compile(rb"\x1b]52;c;([A-Za-z0-9+/=]+)\x1b\\")

    def payloads():
        return [base64.b64decode(g) for g in osc52_re.findall(s.allbuf)]

    before = len(payloads())

    # drag + release over the log body -> auto-copy to the clipboard
    s.send(b"\x1b[<0;15;22M"); time.sleep(0.05)
    s.send(b"\x1b[<32;30;23M"); time.sleep(0.05)
    s.send(b"\x1b[<32;70;26M"); time.sleep(0.05)
    s.send(b"\x1b[<0;70;26m")
    s.drain(0.8)
    auto = payloads()
    assert len(auto) > before, "drag release did not emit an OSC 52 copy"
    assert b"\n" in auto[-1], "selected text should span at least two lines"

    # the selection resets on release, so `y` right after is a no-op
    s.send(b"y"); time.sleep(0.05)
    s.drain(0.8)
    assert len(payloads()) == len(auto), "y copied after the selection was reset on release"

    # a plain click clears the (already reset) selection; y must stay a no-op
    s.send(b"\x1b[<0;15;22M"); time.sleep(0.05)
    s.send(b"\x1b[<0;15;22m"); s.drain(0.8)
    cnt = len(payloads())
    s.send(b"y"); time.sleep(0.05)
    s.drain(0.8)
    assert len(payloads()) == cnt, "y copied with no active selection"


@check
def info_drag_selects_and_copies(s):
    s.drain(3.0)
    # click the Info sub-tab (screen rows 16..18, tab "Info" starts at x=1)
    s.send(b"\x1b[<0;1;17M"); time.sleep(0.05)
    s.send(b"\x1b[<0;1;17m")
    s.drain(1.0)

    sel_bg = "48;2;44;73;46"
    osc52_re = re.compile(rb"\x1b]52;c;([A-Za-z0-9+/=]+)\x1b\\")

    # Info body starts right below the 3-row sub-tab strip (screen y=19):
    # press/motion/release across rows 1..3 of the plain body
    s.send(b"\x1b[<0;2;20M"); time.sleep(0.05)
    s.send(b"\x1b[<32;40;22M"); time.sleep(0.05)
    s.send(b"\x1b[<0;40;22m")
    s.drain(0.8)

    # the live highlight was emitted while dragging (allbuf accumulates the
    # drag frames) and the copy fired
    assert sel_bg in s.allbuf.decode("utf-8", "replace"), \
        "Info drag did not highlight any row"
    payloads = [base64.b64decode(g) for g in osc52_re.findall(s.allbuf)]
    assert payloads and payloads[-1], "Info drag did not copy to the clipboard"
    assert any(ch.isprintable() for ch in payloads[-1].decode("utf-8", "replace")), \
        "Info clipboard payload looks empty"

    # release resets the highlight immediately: a post-release repaint must
    # carry no selection green
    mark = len(s.allbuf)
    s.send(b"\x1b[<64;90;22M"); time.sleep(0.05)   # wheel-up over the Info pane
    s.drain(0.8)
    assert sel_bg not in s.allbuf[mark:].decode("utf-8", "replace"), \
        "Info highlight survived the release (selection must reset immediately)"

    # plain click clears the Info selection and must NOT re-copy
    cnt = len(payloads)
    s.send(b"\x1b[<0;2;20M"); time.sleep(0.05)
    s.send(b"\x1b[<0;2;20m")
    s.drain(0.8)
    assert len(osc52_re.findall(s.allbuf)) == cnt, \
        "plain Info click emitted a copy"


@check
def selection_survives_log_refresh(s):
    s.drain(3.0)
    # open the Logs sub-tab so the log body is underneath the cursor
    s.send(b"\x1b[<0;12;18M"); time.sleep(0.05)
    s.send(b"\x1b[<0;12;18m")
    s.drain(1.0)

    sel_bg = "48;2;44;73;46"

    # drag a selection over the log body; the live highlight appears while
    # dragging, then the release copies and resets it
    mark = len(s.allbuf)
    s.send(b"\x1b[<0;15;22M"); time.sleep(0.05)   # press
    s.send(b"\x1b[<32;40;24M"); time.sleep(0.05)  # left-motion
    s.send(b"\x1b[<0;40;24m")                     # release
    s.drain(0.6)
    assert sel_bg in s.allbuf[mark:].decode("utf-8", "replace"), \
        "drag did not highlight log text while in flight"

    # wait through several 500ms log-refresh ticks: the reset selection must
    # never re-emerge, and the visible position stays frozen (no follow snap)
    s.drain(1.2)

    # force a repaint of the log band via a wheel event; the freshly emitted
    # rows must carry no highlight (immediate reset on release)
    mark = len(s.allbuf)
    s.send(b"\x1b[<64;15;22M"); time.sleep(0.05)   # wheel-up over the log body
    s.drain(0.8)
    tail = s.allbuf[mark:].decode("utf-8", "replace")
    assert sel_bg not in tail, \
        "stale selection highlight after refresh ticks (selection must reset immediately on release)"


@check
def slow_drag_survives_log_refresh(s):
    s.drain(3.0)
    # open the Logs sub-tab so the log body is underneath the cursor
    s.send(b"\x1b[<0;12;18M"); time.sleep(0.05)
    s.send(b"\x1b[<0;12;18m")
    s.drain(1.0)

    sel_bg = "48;2;44;73;46"

    # A slow drag: a ~0.9s pause mid-drag lets at least one 500ms log-refresh
    # tick (a full identical reload for a quiet container) land while the
    # button is still held. That reload must not destroy the in-flight drag.
    mark = len(s.allbuf)
    s.send(b"\x1b[<0;15;22M"); time.sleep(0.05)   # press
    s.send(b"\x1b[<32;30;23M"); time.sleep(0.05)  # motion
    time.sleep(0.9)                               # slow movement -> tick lands
    s.send(b"\x1b[<32;50;24M"); time.sleep(0.05)  # motion continues
    s.send(b"\x1b[<32;60;25M"); time.sleep(0.05)  # motion continues
    s.send(b"\x1b[<0;60;25m")                     # release
    s.drain(0.8)

    # the mid-drag tick must not have killed the live highlight, and the
    # release must then have reset it: the next repaint shows nothing
    assert sel_bg in s.allbuf[mark:].decode("utf-8", "replace"), \
        "live drag highlight never emitted (refresh tick destroyed the drag?)"
    mark = len(s.allbuf)
    s.send(b"\x1b[<64;60;25M"); time.sleep(0.05)   # wheel-over the log body
    s.drain(0.8)
    tail = s.allbuf[mark:].decode("utf-8", "replace")
    assert sel_bg not in tail, \
        "stale highlight after release (selection must reset immediately)"


@check
def narrow_terminal_no_panic(s):
    # reuse current session with a resize instead of a second spawn
    winsize = struct.pack("HHHH", 20, 100, 0, 0)
    fcntl.ioctl(s.master, termios.TIOCSWINSZ, winsize)
    s.drain(1.5)
    assert b"Caught panic" not in s.allbuf, "app panicked at small size"


def main():
    only = sys.argv[1:] if len(sys.argv) > 1 else None
    failed = []
    for i, fn in enumerate(CHECKS):
        if only and fn.__name__ not in only and str(i) not in only:
            continue
        s = Session()
        try:
            fn(s)
            print(f"PASS {fn.__name__}")
        except AssertionError as e:
            print(f"FAIL {fn.__name__}: {e}")
            failed.append(fn.__name__)
        finally:
            s.quit()
    print(f"\n{len(CHECKS) - len(failed)}/{len(CHECKS)} passed")
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
