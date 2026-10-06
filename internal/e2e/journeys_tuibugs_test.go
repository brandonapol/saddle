//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// sendHexApart writes first and then second to the TUI's terminal as two
// writes 10ms apart, so they reach saddle in separate reads the way a slow
// terminal, ssh or tmux can deliver them. tmux runs the three commands in
// order, so the gap does not depend on how fast tmux starts.
func (u *TUI) sendHexApart(first, second string) {
	u.w.T.Helper()
	_, err := u.w.Tmux.Run("send-keys", "-t", u.Target, "-H", first, ";",
		"run-shell", "sleep 0.01", ";",
		"send-keys", "-t", u.Target, "-H", second)
	must(u.w.T, err)
}

// altBracketThenX sends alt+[ then x and Enter to the shell running cat -v,
// and waits for cat to show ^[[x. ESC [ x in one read is a real CSI
// sequence, not alt+[ and x, so when a loaded machine coalesces the writes
// it tries again; each try leaves a 100ms gap for saddle to read alt+[ alone.
func (u *TUI) altBracketThenX() {
	u.w.T.Helper()
	for try := 0; ; try++ {
		u.sendHex("1b", "5b")
		time.Sleep(100 * time.Millisecond)
		u.Type("x")
		u.Keys("Enter")
		for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
			if strings.Contains(u.Screen(), "^[[x") {
				return
			}
		}
		if try == 4 {
			u.waitInShell("^[[x") // fails with the screen
			return
		}
	}
}

// TestJourneyTUIBugs covers four TUI fixes on the real binary:
//   - #206: a merged and a superseded train entry are neither a conflict in
//     the header nor returned in the merge view;
//   - #207: M pressed again right after the first toggle lands flips what the
//     screen shows, not the state the last refresh read;
//   - #201: alt+[ reaches the terminal pane's shell;
//   - #200: alt+` whose ESC and backtick arrive in separate reads still
//     toggles the pane, and no backtick reaches chat.
func TestJourneyTUIBugs(t *testing.T) {
	w := world(t, Options{Tables: "[train]\noutput = \"single\"\n"})
	urls := landTwo(t, w)
	must(t, w.GH.MergeByHand(prNumber(t, w.GHState(), urls["t1"]).Number, "squash"))
	w.MustMCP("t0", "restack", nil)
	if v := w.Task("t1"); !strings.HasPrefix(v.Train, "merged") {
		t.Fatalf("t1 after its PR merged and restack = %+v", v)
	}
	w.MustSaddle("unstack", "t2")
	if v := w.Task("t2"); !strings.HasPrefix(v.Train, "superseded") {
		t.Fatalf("t2 after unstack = %+v", v)
	}

	// #206: the header has no conflict, the merge view nothing returned.
	u := w.StartTUI(160, 44)
	u.WaitScreen("Alpha work", "Beta work")
	if s := u.Screen(); strings.Contains(s, "conflict") {
		t.Fatalf("header counts a merged or superseded entry as a conflict:\n%s", s)
	}
	u.Keys("M-3")
	s := u.WaitScreen("Train", "1 landed", "merged", "superseded")
	if strings.Contains(s, "returned") {
		t.Fatalf("merge view lists a merged or superseded entry as returned:\n%s", s)
	}

	// #207: M on, then M again as soon as the result shows: off.
	u.WaitScreen("auto-merge off")
	u.Type("M")
	u.WaitScreen("auto-merge on")
	u.Type("M")
	u.WaitScreen("auto-merge off")
	if r := w.MustSaddle("automerge", "status"); !strings.Contains(r.Stdout, "auto-merge: off") {
		t.Fatalf("second M left auto-merge on: %s", r)
	}

	// #201: alt+[ then x reaches the shell; cat -v shows it as ^[[x.
	u.Keys("M-1")
	u.WaitScreen("ORCHESTRATOR")
	u.Keys("C-@")
	u.WaitScreen("TERMINAL")
	u.waitInShell("$")
	u.Type("cat -v")
	u.Keys("Enter")
	u.altBracketThenX()
	u.Keys("C-c")

	// #200: ESC and ` in separate reads hide the pane, and type nothing.
	u.sendHexApart("1b", "60")
	u.WaitGone("TERMINAL")
	u.waitNoStrayBacktick()
	u.Quit()
}
