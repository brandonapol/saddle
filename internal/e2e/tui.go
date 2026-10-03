package e2e

import (
	"fmt"
	"strings"
)

// TUI drives `saddle up` running in a pane of the World's private tmux
// server: keys go in through send-keys, and Screen is the rendered terminal
// as tmux's emulator sees it. It is the real binary on a real terminal, so
// it covers Bubble Tea's rendering, alt-screen and resize handling too.
type TUI struct {
	w      *World
	Target string
}

// tuiSession is the tmux session the TUI runs in, apart from saddle's own.
const tuiSession = "e2e-tui"

// StartTUI runs `saddle up --skip-doctor` (plus args) at width x height and
// waits for its first frame. When saddle exits the pane prints its exit
// status and stays, so a crash can be read.
func (w *World) StartTUI(width, height int, args ...string) *TUI {
	w.T.Helper()
	cmd := shq(w.Bins.Saddle) + " up --skip-doctor"
	for _, a := range args {
		cmd += " " + shq(a)
	}
	cmd += `; echo "[saddle up exited $?]"; exec cat`
	must(w.T, w.Tmux.NewSession(tuiSession, width, height, w.Repo, cmd))
	u := &TUI{w: w, Target: tuiSession + ":0"}
	u.WaitScreen("orchestrator")
	return u
}

// Screen is the visible screen.
func (u *TUI) Screen() string {
	u.w.T.Helper()
	s, err := u.w.Tmux.Capture(u.Target)
	must(u.w.T, err)
	return s
}

// Lines is the screen split into rows, trailing blanks trimmed.
func (u *TUI) Lines() []string {
	return strings.Split(strings.TrimRight(u.Screen(), "\n "), "\n")
}

// Keys sends tmux key names.
func (u *TUI) Keys(keys ...string) {
	u.w.T.Helper()
	must(u.w.T, u.w.Tmux.SendKeys(u.Target, keys...))
}

// Type types text literally.
func (u *TUI) Type(text string) {
	u.w.T.Helper()
	must(u.w.T, u.w.Tmux.Type(u.Target, text))
}

// Resize resizes the terminal; Bubble Tea gets SIGWINCH.
func (u *TUI) Resize(width, height int) {
	u.w.T.Helper()
	must(u.w.T, u.w.Tmux.Resize(u.Target, width, height))
}

// WaitScreen waits until the screen contains every one of want.
func (u *TUI) WaitScreen(want ...string) string {
	u.w.T.Helper()
	var last string
	Eventually(u.w.T, fmt.Sprintf("the TUI to show %q", want), func() error {
		s, err := u.w.Tmux.Capture(u.Target)
		if err != nil {
			return err
		}
		last = s
		for _, x := range want {
			if !strings.Contains(s, x) {
				return fmt.Errorf("missing %q; screen:\n%s", x, s)
			}
		}
		return nil
	})
	return last
}

// WaitGone waits until the screen no longer contains s.
func (u *TUI) WaitGone(s string) {
	u.w.T.Helper()
	Eventually(u.w.T, fmt.Sprintf("the TUI to stop showing %q", s), func() error {
		scr, err := u.w.Tmux.Capture(u.Target)
		if err != nil {
			return err
		}
		if strings.Contains(scr, s) {
			return fmt.Errorf("still there; screen:\n%s", scr)
		}
		return nil
	})
}

// Quit presses ctrl+c twice (the first only arms quitting) and waits for
// saddle up to exit.
func (u *TUI) Quit() {
	u.w.T.Helper()
	u.Keys("C-c")
	u.WaitScreen("again to quit")
	u.Keys("C-c")
	u.WaitScreen("[saddle up exited")
}

// Fits reports whether the frame was laid out for the terminal's width
// rather than clipped by it: no row is wider and every box edge that opens
// on a row closes on it.
func (u *TUI) Fits(width int) error {
	for _, l := range u.Lines() {
		l = strings.TrimRight(l, " ")
		r := []rune(l)
		if len(r) > width {
			return fmt.Errorf("row is %d wide: %q", len(r), l)
		}
		if strings.Count(l, "╭") != strings.Count(l, "╮") || strings.Count(l, "╰") != strings.Count(l, "╯") {
			return fmt.Errorf("box edge clipped at %d columns: %q", width, l)
		}
	}
	return nil
}
