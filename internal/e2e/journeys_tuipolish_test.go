//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/e2e/fakeagent"
)

// orchLogHas waits for the fake orchestrator's log to contain want.
func orchLogHas(t *testing.T, w *World, want string) {
	t.Helper()
	Eventually(t, "the orchestrator log to show "+want, func() error {
		b, _ := os.ReadFile(fakeagent.OrchestratorLog(w.Scripts))
		if !strings.Contains(string(b), want) {
			return errorf("orchestrator log:\n%s", b)
		}
		return nil
	})
}

// #265: Esc in the chat, or one ctrl+c, interrupts a turn stuck in a tool
// call. The chat says Interrupted, the same session takes the next message,
// and saddle up keeps running.
func TestJourneyInterruptOrchestrator(t *testing.T) {
	w := world(t, Options{})
	u := w.StartTUI(140, 40)
	u.WaitScreen("ORCHESTRATOR")

	u.Type("slow: build the thing")
	u.Keys("Enter")
	u.WaitScreen("Bash sleep 600", "esc to interrupt")
	u.Keys("Escape")
	orchLogHas(t, w, "control: interrupt")
	u.WaitScreen("Interrupted.")
	u.WaitGone("esc to interrupt")

	u.Type("hello again")
	u.Keys("Enter")
	u.WaitScreen("fake orchestrator ack: hello again")

	// ctrl+c once interrupts and arms quitting; it doesn't quit.
	u.Type("slow: second")
	u.Keys("Enter")
	u.WaitScreen("esc to interrupt")
	u.Keys("C-c")
	u.WaitScreen("Ctrl+C again to quit")
	Eventually(t, "two interrupts", func() error {
		b, _ := os.ReadFile(fakeagent.OrchestratorLog(w.Scripts))
		if n := strings.Count(string(b), "control: interrupt"); n != 2 {
			return errorf("%d interrupts in:\n%s", n, b)
		}
		return nil
	})
	u.WaitGone("esc to interrupt")
	u.Quit()
}

// tmuxShim puts a tmux on the world's PATH that logs each call saddle
// makes, then runs the real one. It returns the log's path.
func tmuxShim(t *testing.T, w *World) string {
	t.Helper()
	real, err := exec.LookPath("tmux")
	must(t, err)
	log := filepath.Join(w.Root, "tmux-calls.log")
	shim := "#!/bin/sh\n" +
		"[ \"$(cat /proc/$PPID/comm 2>/dev/null)\" = saddle ] && echo \"$1\" >> " + shq(log) + "\n" +
		"exec " + shq(real) + " \"$@\"\n"
	must(t, os.WriteFile(filepath.Join(w.Bin, "tmux"), []byte(shim), 0o755))
	return log
}

// countCalls counts the tmux subcommands in the shim's log.
func countCalls(log string) int {
	b, _ := os.ReadFile(log)
	n := 0
	for _, l := range strings.Split(string(b), "\n") {
		if l == "list-windows" || l == "capture-pane" || l == "display-message" {
			n++
		}
	}
	return n
}

// #272: with idle agents, saddle up's refresh starts a fixed number of tmux
// processes per tick (one window list, one batched capture), not two per
// agent.
func TestJourneyRefreshTmuxExecs(t *testing.T) {
	if _, err := os.Stat("/proc/self/comm"); err != nil {
		t.Skip("needs /proc to tell saddle's tmux calls apart")
	}
	w := world(t, Options{})
	for _, id := range []string{"t1", "t2", "t3", "t4", "t5"} { // the default cap
		idleAgent(w, id, "Idle agent "+id, "dir"+id+"/**")
	}
	log := tmuxShim(t, w)
	u := w.StartTUI(160, 44)
	u.WaitScreen("Idle agent t5")
	time.Sleep(2 * time.Second) // past start-up
	before := countCalls(log)
	const secs = 6
	time.Sleep(secs * time.Second)
	calls := countCalls(log) - before
	// Two per one-second tick, plus slack for a refresh a key or the
	// orchestrator triggered. The old refresh made 2 per agent: 60 here.
	if calls > 2*secs+6 {
		b, _ := os.ReadFile(log)
		t.Fatalf("saddle up ran %d tmux processes in %ds with 5 idle agents; want about %d. Calls:\n%s", calls, secs, 2*secs, b)
	}
	if calls == 0 {
		t.Fatal("the shim saw no tmux calls from saddle up; the count proves nothing")
	}
	u.Quit()
}
