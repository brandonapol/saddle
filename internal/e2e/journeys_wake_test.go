//go:build e2e

package e2e

import (
	"os"
	"strings"
	"testing"

	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
	"github.com/brandonapol/saddle/internal/mcpserver"
)

const wakeText = "You have new notices"

// paint writes s to the terminal of window target, as the program in it
// would draw it, without the program reading it as input.
func (w *World) paint(target, s string) {
	w.T.Helper()
	tty, err := w.Tmux.Run("display-message", "-p", "-t", target, "#{pane_tty}")
	must(w.T, err)
	f, err := os.OpenFile(strings.TrimSpace(tty), os.O_WRONLY, 0)
	must(w.T, err)
	defer f.Close()
	_, err = f.WriteString(s)
	must(w.T, err)
}

// waitScreen waits until window target's screen contains want.
func (w *World) waitScreen(target, want string) string {
	w.T.Helper()
	var last string
	Eventually(w.T, target+" to show "+want, func() error {
		s, err := w.Tmux.Capture(target)
		if err != nil {
			return err
		}
		last = s
		if !strings.Contains(s, want) {
			return errorf("screen:\n%s", s)
		}
		return nil
	})
	return last
}

// waitPrompt waits until window target's last line is an empty prompt.
func (w *World) waitPrompt(target string) {
	w.T.Helper()
	Eventually(w.T, target+" to show an empty prompt", func() error {
		s, err := w.Tmux.Capture(target)
		if err != nil {
			return err
		}
		ls := strings.Split(strings.TrimRight(s, "\n "), "\n")
		if strings.TrimSpace(ls[len(ls)-1]) != ">" {
			return errorf("screen:\n%s", s)
		}
		return nil
	})
}

// TestJourneyWakeIdleAgentWithPromptSuggestion (#183): Claude Code fills an
// idle agent's empty prompt with a grey suggestion of what to do next. That
// is not the user drafting: the wake line must still reach the agent, so a
// notice (here a message) doesn't sit undelivered while it idles.
func TestJourneyWakeIdleAgentWithPromptSuggestion(t *testing.T) {
	w := world(t, Options{})
	w.Spawn("t1", "Alpha work", []string{"alpha/**"}, fa.Wait("carry on with alpha"))
	v := w.WaitStatus("t1", "idle")
	w.waitPrompt(v.Window)
	// Faint text after the prompt, as Claude Code draws its suggestion.
	w.paint(v.Window, "\x1b[2mrun saddle restack now\x1b[0m")
	w.waitScreen(v.Window, "> run saddle restack now")

	w.MustSaddle("message", "t1", "carry on with alpha")
	w.WaitAgentLog("t1", "input: [saddle] "+wakeText)
	w.WaitAgentLog("t1", "step: 1 wait carry on with alpha ok")
}

// TestJourneyWakeLineNeverTypedIntoForeignWindow: a task's recorded window
// can stop being its agent's: the user reuses it for a shell, or a tmux
// restart hands its id to another window, even the TUI's own. A notice for
// the task must not type the wake line into whatever is there now; it waits
// as a notice instead.
func TestJourneyWakeLineNeverTypedIntoForeignWindow(t *testing.T) {
	w := world(t, Options{})
	w.Spawn("t1", "Alpha work", []string{"alpha/**"}, fa.Wait("never-comes"))
	v := w.WaitStatus("t1", "idle")

	// The user takes the window over for something else.
	_, err := w.Tmux.Run("respawn-pane", "-k", "-t", v.Window, "cat")
	must(t, err)
	_, err = w.Tmux.Run("rename-window", "-t", v.Window, "scratch")
	must(t, err)
	w.MustSaddle("message", "t1", "first note")
	// tmux delivers input in order: once the probe echoes, a wake line typed
	// before it would be on screen too.
	must(t, w.Tmux.Type(v.Window, "probe-one"))
	if s := w.waitScreen(v.Window, "probe-one"); strings.Contains(s, wakeText) {
		t.Fatalf("the wake line was typed into a window that isn't t1's agent:\n%s", s)
	}

	// The record now points at the TUI's window.
	u := w.StartTUI(120, 36)
	tuiWin, err := w.Tmux.Run("display-message", "-p", "-t", u.Target, "#{window_id}")
	must(t, err)
	must(t, w.App().Store.SetField("t1", "window", tuiWin))
	w.MustSaddle("message", "t1", "second note")
	u.Type("probe-two")
	u.WaitScreen("probe-two")
	if s := u.Screen(); strings.Contains(s, wakeText) {
		t.Fatalf("the wake line was typed into the TUI:\n%s", s)
	}
	if n, err := w.App().Store.PendingNotices("t1"); err != nil || n < 2 {
		t.Fatalf("t1's notices = %d (%v), want both still pending", n, err)
	}
	u.clearInput("probe-two")
	u.Quit()
}

// TestJourneyTestFailureWakesIdleProducer: the train's test gate fails an
// idle agent's branch. The failure goes back to that agent, which is woken
// at its prompt, fixes the branch, calls done again and lands.
func TestJourneyTestFailureWakesIdleProducer(t *testing.T) {
	w := world(t, Options{})
	w.Spawn("t1", "Alpha work", []string{"alpha/**"},
		fa.Write("alpha/work.txt", "alpha\n"), fa.Write("FAIL_TESTS", "x\n"), fa.Commit("alpha, tests red"), fa.Done("Adds alpha."),
		fa.Wait("failed on the result"),
		fa.Step{Remove: "FAIL_TESTS"}, fa.Commit("fix the tests"), fa.Done("Adds alpha, tests green."))
	w.WaitTask("t1", "queued", func(v mcpserver.TaskView) bool { return v.Train == "queued" })
	w.WaitStatus("t1", "done")

	w.Saddle("land")
	if v := w.Task("t1"); v.Status == "landed" {
		t.Fatalf("t1 landed with failing tests: %+v", v)
	}
	w.WaitAgentLog("t1", "input: [saddle] "+wakeText)
	w.WaitAgentLog("t1", "idle: script finished")
	w.WaitTask("t1", "queued again", func(v mcpserver.TaskView) bool { return v.Train == "queued" })
	w.MustSaddle("land")
	if v := w.Task("t1"); v.Status != "landed" {
		t.Fatalf("t1 after fixing its tests = %+v", v)
	}
}
