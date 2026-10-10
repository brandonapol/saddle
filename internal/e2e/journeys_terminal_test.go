//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
)

// The terminal pane's toggle as terminals deliver it. Legacy terminals send
// alt+` as ESC then the backtick, and ctrl+` as NUL; terminals speaking the
// kitty keyboard protocol or xterm's modifyOtherKeys send CSI sequences.
var (
	altBacktickLegacy   = []string{"1b", "60"}
	altBacktickCSIu     = hexOf("\x1b[96;3u")
	ctrlBacktickCSIu    = hexOf("\x1b[96;5u")
	ctrlBacktickModKeys = hexOf("\x1b[27;5;96~")
)

func hexOf(s string) []string {
	var out []string
	for _, b := range []byte(s) {
		out = append(out, fmt.Sprintf("%02x", b))
	}
	return out
}

// sendHex writes raw bytes to the TUI's terminal, in one write.
func (u *TUI) sendHex(bytes ...string) {
	u.w.T.Helper()
	_, err := u.w.Tmux.Run(append([]string{"send-keys", "-t", u.Target, "-H"}, bytes...)...)
	must(u.w.T, err)
}

// termRow is the screen row of the terminal pane's title, or -1 when the
// pane is not shown. Rows below it are the pane.
func termRow(lines []string) int {
	for i, l := range lines {
		if strings.Contains(l, "TERMINAL") {
			return i
		}
	}
	return -1
}

// waitInChat waits until text shows above the terminal pane (in the chat
// input) and nowhere in it.
func (u *TUI) waitInChat(text string) {
	u.w.T.Helper()
	Eventually(u.w.T, "\""+text+"\" to land in the chat input, not the shell", func() error {
		ls := u.Lines()
		row := termRow(ls)
		for i, l := range ls {
			if strings.Contains(l, text) {
				if row >= 0 && i > row {
					return errorf("%q went to the shell; screen:\n%s", text, strings.Join(ls, "\n"))
				}
				return nil
			}
		}
		return errorf("%q not on screen:\n%s", text, strings.Join(ls, "\n"))
	})
}

// waitInShell waits until text shows inside the terminal pane.
func (u *TUI) waitInShell(text string) {
	u.w.T.Helper()
	Eventually(u.w.T, "\""+text+"\" to show in the shell", func() error {
		ls := u.Lines()
		row := termRow(ls)
		if row < 0 {
			return errorf("no terminal pane; screen:\n%s", strings.Join(ls, "\n"))
		}
		for _, l := range ls[row+1:] {
			if strings.Contains(l, text) {
				return nil
			}
		}
		return errorf("%q not in the pane; screen:\n%s", text, strings.Join(ls, "\n"))
	})
}

// clearInput empties the chat input (ctrl+u deletes to the line start).
func (u *TUI) clearInput(text string) {
	u.w.T.Helper()
	u.Keys("C-u")
	u.WaitGone(text)
}

// TestJourneyTerminalPane walks the embedded terminal (#177) with the toggle
// as legacy terminals and tmux send ctrl+` (NUL): the shell runs and resizes
// with the window, esc hands keys back to chat without eating what is typed
// next, the shell survives hiding, and the TUI carries on after the shell
// exits.
func TestJourneyTerminalPane(t *testing.T) {
	w := world(t, Options{})
	u := w.StartTUI(120, 36)

	u.Keys("C-@")
	u.WaitScreen("TERMINAL")
	u.waitInShell("$")
	u.Type("echo hello-$((6*7))")
	u.Keys("Enter")
	u.waitInShell("hello-42")

	// Resizing while open resizes the shell's pty too.
	u.Resize(100, 40)
	Eventually(t, "the TUI to lay out for 100 columns", func() error { return u.Fits(100) })
	u.Type("stty size")
	u.Keys("Enter")
	u.waitInShell(" 98")

	// esc returns focus to chat, and what is typed right after lands there.
	u.Keys("Escape")
	u.Type("typed-after-esc")
	u.waitInChat("typed-after-esc")
	u.clearInput("typed-after-esc")

	// From chat the toggle refocuses the pane; from the pane it hides it.
	u.Keys("C-@")
	u.Type("echo refocused")
	u.Keys("Enter")
	u.waitInShell("refocused")
	u.Keys("C-@")
	u.WaitGone("TERMINAL")

	// Reopening keeps the running shell and what it printed.
	u.Keys("C-@")
	u.waitInShell("hello-42")

	// ctrl+d exits the shell: the pane closes and chat has focus.
	u.Keys("C-d")
	u.WaitScreen("shell exited")
	u.WaitGone("TERMINAL")
	u.Type("after-exit")
	u.waitInChat("after-exit")
	u.clearInput("after-exit")

	// The toggle starts a fresh shell.
	u.Keys("C-@")
	u.WaitScreen("TERMINAL")
	u.waitInShell("$")
	u.Type("echo second-shell")
	u.Keys("Enter")
	u.waitInShell("second-shell")
	u.Keys("Escape")

	u.Quit()
}

// TestJourneyTerminalToggleEncodings sends the toggle every way terminals
// encode it: alt+` as ESC ` and as CSI u, ctrl+` as CSI u and as
// modifyOtherKeys. Each must open or hide the pane and none may leak into the
// shell or the chat input. (ESC and backtick arriving in separate reads
// still split: #200.)
func TestJourneyTerminalToggleEncodings(t *testing.T) {
	w := world(t, Options{})
	u := w.StartTUI(120, 36)

	u.sendHex(altBacktickLegacy...)
	u.WaitScreen("TERMINAL")
	u.waitInShell("$")
	u.Type("echo marker")
	u.Keys("Enter")
	u.waitInShell("marker")

	u.sendHex(altBacktickLegacy...)
	u.WaitGone("TERMINAL")
	u.waitNoStrayBacktick()

	u.sendHex(altBacktickCSIu...)
	u.waitInShell("marker")
	u.sendHex(altBacktickCSIu...)
	u.WaitGone("TERMINAL")

	u.sendHex(ctrlBacktickCSIu...)
	u.waitInShell("marker")
	u.sendHex(ctrlBacktickModKeys...)
	u.WaitGone("TERMINAL")
	u.sendHex(ctrlBacktickModKeys...)
	u.waitInShell("marker")

	// The shell got none of the toggles as input.
	u.Type("echo clean")
	u.Keys("Enter")
	u.waitInShell("clean")
	if s := u.Screen(); strings.Contains(s, "[96;") || strings.Contains(s, "96~") {
		t.Fatalf("a toggle sequence leaked into the shell:\n%s", s)
	}
	u.Keys("Escape")
	u.WaitGone("esc chat") // wait for terminal focus to return before Ctrl+C
	u.Quit()
}

// waitNoStrayBacktick checks the toggle did not also type a backtick into
// the chat input.
func (u *TUI) waitNoStrayBacktick() {
	u.w.T.Helper()
	u.Type("probe")
	u.waitInChat("probe")
	if s := u.Screen(); strings.Contains(s, "`probe") {
		u.w.T.Fatalf("the toggle typed a backtick into chat:\n%s", s)
	}
	u.clearInput("probe")
}
