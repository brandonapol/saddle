//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// startTUIWayland is StartTUI with WAYLAND_DISPLAY set, so saddle picks
// wl-copy, which this World fakes on PATH.
func (w *World) startTUIWayland(width, height int) *TUI {
	w.T.Helper()
	cmd := "env WAYLAND_DISPLAY=e2e-wayland " + shq(w.Bins.Saddle) + ` up --skip-doctor; echo "[saddle up exited $?]"; exec cat`
	must(w.T, w.Tmux.NewSession(tuiSession, width, height, w.Repo, cmd))
	u := &TUI{w: w, Target: tuiSession + ":0"}
	u.WaitScreen("orchestrator")
	return u
}

// fakeWlCopy puts a wl-copy on the World's PATH that saves its stdin to the
// returned file.
func fakeWlCopy(w *World) string {
	out := filepath.Join(w.Root, "clipboard.txt")
	script := fmt.Sprintf("#!/bin/sh\ncat > %s.tmp && mv %s.tmp %s\n", shq(out), shq(out), shq(out))
	must(w.T, os.WriteFile(filepath.Join(w.Bin, "wl-copy"), []byte(script), 0o755))
	return out
}

// waitClipboard waits until the fake wl-copy and tmux's paste buffer both
// hold want.
func waitClipboard(t *testing.T, w *World, file, want string) {
	t.Helper()
	Eventually(t, fmt.Sprintf("the clipboard to hold %q", want), func() error {
		b, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		if string(b) != want {
			return errorf("wl-copy got %q", b)
		}
		buf, err := w.Tmux.Run("show-buffer")
		if err != nil {
			return err
		}
		if buf != want && strings.TrimSuffix(buf, "\n") != want {
			return errorf("tmux buffer holds %q", buf)
		}
		return nil
	})
}

// rowOf is the screen row and column of the last row containing s, or -1.
func rowOf(lines []string, s string) (x, y int) {
	for i := len(lines) - 1; i >= 0; i-- {
		if j := strings.Index(lines[i], s); j >= 0 {
			return len([]rune(lines[i][:j])), i
		}
	}
	return -1, -1
}

// rowAlone is like rowOf for a row where s is all the text inside one box
// (copy mode's cursor aside).
func rowAlone(lines []string, s string) (x, y int) {
	for i := len(lines) - 1; i >= 0; i-- {
		if slices.ContainsFunc(strings.Split(lines[i], "│"), func(f string) bool { return strings.Trim(f, " ›") == s }) {
			x, _ := rowOf(lines[i:i+1], s)
			return x, i
		}
	}
	return -1, -1
}

// copyPos is copy mode's "cursor/lines" counter from its title, or "".
func copyPos(lines []string) string {
	for _, l := range lines {
		if _, rest, ok := strings.Cut(l, "COPY · "); ok {
			if f := strings.Fields(strings.ReplaceAll(rest, "·", " ")); len(f) >= 2 {
				return f[1]
			}
		}
	}
	return ""
}

// stepCopyCursor presses key once in copy mode and gives the cursor a
// moment to move off the position shown in before, so the next look sees
// where it went. A key that cannot move it (k on the top line) just times
// out here; the caller looks again either way.
func stepCopyCursor(u *TUI, before []string, key string) {
	was := copyPos(before)
	u.Keys(key)
	_ = Poll(time.Second, func() error {
		if p := copyPos(u.Lines()); p == was {
			return errorf("cursor still at %s", p)
		}
		return nil
	})
}

// TestJourneyCopyToClipboard copies chat lines out of the TUI (#217): in
// copy mode, the cursor moves to a message, V and k select it and its
// header, y copies; then a mouse drag over the same rows copies them again.
// Both land in the fake wl-copy and in tmux's paste buffer, as plain text.
func TestJourneyCopyToClipboard(t *testing.T) {
	w := world(t, Options{})
	clip := fakeWlCopy(w)
	u := w.startTUIWayland(120, 36)

	const msg = "copy-me-217"
	u.Type(msg)
	u.Keys("Enter")
	u.WaitScreen("you", msg)

	u.Keys("C-y")
	u.WaitScreen("COPY · chat")
	// Step the cursor one row at a time, rereading the screen each time: a
	// capture taken mid-redraw can still hold the chat view's input prompt,
	// so a count of k presses worked out from one capture can overshoot.
	Eventually(t, "the cursor to reach the message", func() error {
		ls := u.Lines()
		_, cur := rowOf(ls, "› ")
		_, at := rowAlone(ls, msg)
		switch {
		case cur < 0 || at < 0:
			return errorf("cursor row %d, message row %d:\n%s", cur, at, strings.Join(ls, "\n"))
		case at < cur:
			stepCopyCursor(u, ls, "k")
		case at > cur:
			stepCopyCursor(u, ls, "j")
		case strings.Contains(ls[cur], "› "+msg+" "):
			return nil
		}
		return errorf("cursor not on %q:\n%s", msg, strings.Join(ls, "\n"))
	})
	u.Keys("V", "k")
	u.WaitScreen("2 selected")
	u.Keys("y")
	u.WaitScreen("copied 2 lines")
	u.WaitGone("COPY · chat")
	waitClipboard(t, w, clip, "you\n"+msg)

	// A drag from the message up to its header copies the same two rows.
	must(t, os.Remove(clip))
	_, err := w.Tmux.Run("delete-buffer")
	must(t, err)
	x, y := rowAlone(u.Lines(), msg)
	if y < 1 {
		t.Fatalf("%q not on screen:\n%s", msg, u.Screen())
	}
	// SGR mouse reports are 1-based: press, motion with the button held, release.
	u.sendHex(hexOf(fmt.Sprintf("\x1b[<0;%d;%dM", x+1, y+1))...)
	u.sendHex(hexOf(fmt.Sprintf("\x1b[<32;%d;%dM", x+1, y))...)
	u.sendHex(hexOf(fmt.Sprintf("\x1b[<0;%d;%dm", x+1, y))...)
	waitClipboard(t, w, clip, "you\n"+msg)

	// Nothing leaked into the chat input.
	if s := u.Screen(); strings.Contains(s, "[<") {
		t.Fatalf("a mouse report leaked into the input:\n%s", s)
	}
	u.Quit()
}
