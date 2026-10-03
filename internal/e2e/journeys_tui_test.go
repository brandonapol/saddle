//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
	"github.com/brandonapol/saddle/internal/e2e/fakegh"
	"github.com/brandonapol/saddle/internal/usage"
)

// row is the first screen row containing s, or "".
func (u *TUI) row(s string) string {
	for _, l := range u.Lines() {
		if strings.Contains(l, s) {
			return l
		}
	}
	return ""
}

// waitHeader waits until the header row contains s. The header shows the
// state the TUI last read, unlike a flash, which shows an action began; a
// toggle pressed before the state is read toggles the stale state (#207).
func (u *TUI) waitHeader(s string) {
	u.w.T.Helper()
	Eventually(u.w.T, "the header to show "+s, func() error {
		if h := u.Lines()[0]; !strings.Contains(h, s) {
			return errorf("header: %q", h)
		}
		return nil
	})
}

// waitFits waits until the frame is laid out for width.
func (u *TUI) waitFits(width int) {
	u.w.T.Helper()
	Eventually(u.w.T, "the TUI to lay out for "+itoa(width)+" columns", func() error { return u.Fits(width) })
}

// TestJourneyTUIViewRouter: alt+1/2/3 switch between the control, plan and
// merge views at 120 and at 36 columns, every view fits the width, and the
// keys never type into the chat input.
func TestJourneyTUIViewRouter(t *testing.T) {
	w := world(t, Options{})
	u := w.StartTUI(120, 40)
	for _, width := range []int{120, 36} {
		u.Resize(width, 40)
		u.waitFits(width)

		u.Keys("M-2")
		u.WaitScreen("No plan under review")
		u.waitFits(width)
		u.Keys("M-3")
		u.WaitScreen("The train is empty")
		u.waitFits(width)
		if width >= 74 {
			u.WaitScreen("MERGE TRAIN", "auto-merge off")
		}
		u.Keys("M-1")
		u.WaitGone("The train is empty")
		u.waitFits(width)
		if width >= 74 {
			u.WaitScreen("ORCHESTRATOR", "AGENTS")
		}
		// The current view is named in the header, as a tab when they fit.
		if width >= 74 {
			if h := u.Lines()[0]; !strings.Contains(h, "1 control") || !strings.Contains(h, "3 merge") {
				t.Fatalf("header at %d columns lacks the tabs: %q", width, h)
			}
		} else if h := u.Lines()[0]; !strings.Contains(h, "control") {
			t.Fatalf("header at %d columns doesn't name the view: %q", width, h)
		}
	}
	// None of the view keys reached the chat input.
	u.Type("probe")
	u.waitInChat("probe")
	if s := u.Screen(); strings.Contains(s, "123probe") || strings.Contains(s, "3probe") || strings.Contains(s, "2probe") {
		t.Fatalf("a view key typed into chat:\n%s", s)
	}
	u.clearInput("probe")
	u.Quit()
}

// idleTwo spawns two agents that idle at their prompts.
func idleTwo(t *testing.T, w *World) {
	t.Helper()
	w.Spawn("t1", "Alpha work", []string{"alpha/**"}, fa.Wait("never-comes"))
	w.Spawn("t2", "Beta work", []string{"beta/**"}, fa.Wait("never-comes"))
	for _, id := range []string{"t1", "t2"} {
		w.WaitStatus(id, "idle")
	}
}

// TestJourneyTUIPeekCycleAndBrief: the peek shows the selected agent's
// terminal and says how to switch; alt+n and alt+p cycle through the live
// agents from the chat or the agent list, b swaps the peek for the agent's
// brief and back, and at 36 columns tab shows the agents and peek alone.
func TestJourneyTUIPeekCycleAndBrief(t *testing.T) {
	w := world(t, Options{})
	idleTwo(t, w)
	u := w.StartTUI(120, 40)

	u.WaitScreen("PEEK 1/2 alt+n/p")
	first, second := "t1", "t2"
	if strings.Contains(u.row("PEEK"), "t2 Beta work") {
		first, second = second, first
	}
	u.WaitScreen("[fakeagent " + first + "] prompt: Prompt for")

	// From the chat.
	u.Keys("M-n")
	u.WaitScreen("PEEK 2/2 alt+n/p · " + second)
	u.WaitScreen("[fakeagent " + second + "] prompt: Prompt for")
	if strings.Contains(u.Screen(), "[fakeagent "+first+"]") {
		t.Fatalf("the peek still shows %s after alt+n:\n%s", first, u.Screen())
	}
	u.Keys("M-n") // wraps around
	u.WaitScreen("PEEK 1/2 alt+n/p · " + first)

	// From the agent list.
	u.Keys("Tab")
	u.Keys("M-p")
	u.WaitScreen("PEEK 2/2 alt+n/p · " + second)

	u.Keys("b")
	u.WaitScreen("BRIEF · " + second)
	u.WaitScreen("Prompt for")
	u.WaitGone("[fakeagent " + second + "] prompt")
	u.Keys("b")
	u.WaitScreen("PEEK 2/2")
	u.WaitScreen("[fakeagent " + second + "] prompt")

	u.Resize(36, 40)
	u.waitFits(36)
	u.WaitScreen("PEEK", "AGENTS")
	u.Keys("Tab")
	u.WaitGone("AGENTS")
	u.waitFits(36)
	u.Quit()
}

// helpDescs is every binding's description in the help overlay.
var helpDescs = []string{
	"views", "plan view", "merge view", "keys", "next/prev agent", "prev agent", "switch pane", "scroll up", "scroll down",
	"restart orchestrator", "quit (agents keep running)",
	"send", "newline", "complete", "back to orchestrator", "ask narrator", "send agent's screen",
	"select", "up", "open window", "skill in agent", "spawn", "pause (esc)", "brief/peek", "kill", "land", "orchestrator",
	"approve/reopen", "edit in $EDITOR", "replan with a note", "task model", "task model down", "go: run the plan",
	"next/prev plan", "prev plan", "bots limit", "bots limit down",
	"auto-merge on/off", "hold/release", "rebase stack", "move in queue", "move up in queue", "take over (open agent)",
	"terminal", "chat", "history",
}

// TestJourneyTUIHelpOverlay: f1 opens the key help from the chat (where ?
// is a character) and lists every group and binding; ? opens it from the
// agent list; esc closes it; it fits a 36-column terminal.
func TestJourneyTUIHelpOverlay(t *testing.T) {
	w := world(t, Options{})
	u := w.StartTUI(160, 60)

	u.Keys("F1")
	u.WaitScreen("KEYS")
	s := u.Screen()
	for _, g := range []string{"Anywhere", "Chat", "Agents", "Plan view", "Merge view", "Terminal"} {
		if !strings.Contains(s, g) {
			t.Errorf("help lacks the %q group", g)
		}
	}
	for _, d := range helpDescs {
		if !strings.Contains(s, d) {
			t.Errorf("help lacks %q", d)
		}
	}
	if t.Failed() {
		t.Fatalf("help overlay:\n%s", s)
	}
	u.Keys("Escape")
	u.WaitGone("KEYS")

	// In the chat ? is typed; from the agent list it opens help.
	u.Type("?")
	u.waitInChat("?")
	u.clearInput("› ?")
	u.Keys("Tab")
	u.Type("?")
	u.WaitScreen("KEYS")
	u.Resize(36, 40)
	u.waitFits(36)
	u.WaitScreen("KEYS")
	u.Keys("?")
	u.WaitGone("KEYS")
	u.Quit()
}

// TestJourneyTUIMergeViewKeys: in the merge view, M turns auto-merge on and
// off, h holds and releases the selected stack, and r rebases it onto a
// main that moved; each shows in the view and in saddle automerge status.
func TestJourneyTUIMergeViewKeys(t *testing.T) {
	w := world(t, Options{})
	urls := landThree(t, w)
	// CI stays pending, so nothing merges while auto-merge is on.
	must(t, w.GH.SetAllChecks(fakegh.Pending))
	w.AutomergeTick() // the watcher's first check, which saddle up also runs at start
	u := w.StartTUI(120, 45)

	u.Keys("M-3")
	u.WaitScreen("auto-merge off", "stack t1", "stack t2", "stack t3")
	u.Keys("M")
	u.waitHeader("auto-merge on")
	if out := w.MustSaddle("automerge", "status").Stdout; !strings.Contains(out, "on") || strings.Contains(out, "auto-merge is off") {
		t.Fatalf("status after M:\n%s", out)
	}
	u.Keys("M")
	u.waitHeader("auto-merge off")
	if st := w.AutomergeTick(); st.Enabled {
		t.Fatalf("auto-merge still on after the second M: %+v", st)
	}

	u.Keys("j")
	u.Keys("h")
	u.WaitScreen("held stack t2")
	u.WaitScreen("1 held")
	if st := w.AutomergeTick(); !strings.Contains(strings.Join(st.Holds, ","), "t2") {
		t.Fatalf("holds after h = %q, want t2", st.Holds)
	}
	u.Keys("h")
	u.WaitScreen("released stack t2")
	if st := w.AutomergeTick(); len(st.Holds) != 0 {
		t.Fatalf("holds after the second h = %q", st.Holds)
	}

	// main moves under t2: r rebases its stack onto it.
	must(t, w.GH.MergeByHand(prNumber(t, w.GHState(), urls["t1"]).Number, "squash"))
	head := w.Task("t2").Branch
	u.Keys("r")
	u.WaitScreen("rebased stack t2")
	w.Git(w.Repo, "fetch", "-q", "origin")
	if mb, main := w.Git(w.Repo, "merge-base", "origin/main", "origin/"+head), w.Git(w.Repo, "rev-parse", "origin/main"); mb != main {
		t.Fatalf("t2's branch isn't on the new main after r")
	}
	if files := w.Git(w.Repo, "diff", "--name-only", "origin/main...origin/"+head); files != "beta/work.txt" {
		t.Fatalf("t2's PR diff after r = %q", files)
	}
	u.Quit()
}

// TestJourneyRebaseStackIsIdempotent (#205): r in the merge view
// (saddle stack rebase) on a stack already on main must move nothing and
// say so. Today every call rebuilds every branch with new SHAs, so each
// press force-moves the PR heads (restarting their CI) and "already on main"
// never shows.
func TestJourneyRebaseStackIsIdempotent(t *testing.T) {
	t.Skip("#205: saddle stack rebase rewrites every branch on every call")
	w := world(t, Options{})
	landThree(t, w)
	must(t, w.GH.MergeByHand(prNumber(t, w.GHState(), w.Task("t1").PR).Number, "squash"))
	w.MustSaddle("stack", "rebase", "t2")
	branch := w.Task("t2").Branch
	before := w.Git(w.Repo, "rev-parse", branch)
	if r := w.MustSaddle("stack", "rebase", "t2"); !strings.Contains(r.Stdout, "already on main") {
		t.Errorf("second rebase of a stack on main: %s", r)
	}
	if after := w.Git(w.Repo, "rev-parse", branch); after != before {
		t.Fatalf("second rebase moved %s %s -> %s with main unchanged", branch, before[:8], after[:8])
	}

	w.AutomergeTick()
	u := w.StartTUI(120, 45)
	u.Keys("M-3")
	u.WaitScreen("stack t2")
	u.Keys("r")
	u.WaitScreen("stack t2 is already on main")
	u.Quit()
}

// TestJourneyTUIMergedTaskNotShownAsConflict (#206): once a task's PR
// merges its train entry is "merged". The merge view must list it as done,
// not under returned, and the header must not count it as a conflict.
func TestJourneyTUIMergedTaskNotShownAsConflict(t *testing.T) {
	t.Skip("#206: merged train entries show as returned and count as conflicts")
	w := world(t, Options{})
	urls := landThree(t, w)
	must(t, w.GH.MergeByHand(prNumber(t, w.GHState(), urls["t1"]).Number, "squash"))
	w.MustMCP("t0", "restack", nil)
	if v := w.Task("t1"); !strings.HasPrefix(v.Train, "merged") {
		t.Fatalf("t1 after its PR merged = %+v", v)
	}
	u := w.StartTUI(120, 45)
	u.WaitScreen("3 landed")
	if h := u.Lines()[0]; strings.Contains(h, "conflict") {
		t.Errorf("header counts the merged t1 as a conflict: %q", h)
	}
	u.Keys("M-3")
	u.WaitScreen("Train")
	if s := u.Screen(); strings.Contains(s, "returned") {
		t.Errorf("the merge view lists the merged t1 as returned:\n%s", s)
	}
	u.Quit()
}

// TestJourneyTUIUsageStrip: with a five-hour cap configured, usage near it
// shows above the footer as a bar, percent and warning, and the 60-minute
// usage box totals it by model.
func TestJourneyTUIUsageStrip(t *testing.T) {
	w := world(t, Options{Tables: "[limits.five_hour]\ntokens = 1000000\n"})
	now := time.Now().UTC().Truncate(time.Minute)
	must(t, w.App().Store.AddUsage("e2e-usage", false, []usage.Bucket{{
		Key:    usage.Key{Minute: now, Task: "t0", Model: "claude-opus-4-5"},
		Tokens: usage.Tokens{Input: 400000, Output: 450000}, Messages: 3,
	}}))
	u := w.StartTUI(120, 45)
	u.WaitScreen("85%", "850.0k tok", "USAGE · 60m", "opus 850.0k")
	strip := u.row("85%")
	if !strings.Contains(strip, "warn") || !strings.Contains(strip, "resets") {
		t.Fatalf("usage strip = %q, want a warning and the reset time", strip)
	}
	if err := u.Fits(120); err != nil {
		t.Fatal(err)
	}
	u.Resize(36, 40)
	u.waitFits(36)
	u.WaitScreen("85%")
	u.Quit()
}

