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
	"github.com/brandonapol/saddle/internal/mcpserver"
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

// startTUIEnv is StartTUI with extra environment, like NO_COLOR=1.
func (w *World) startTUIEnv(width, height int, env string) *TUI {
	w.T.Helper()
	cmd := "env " + env + " " + shq(w.Bins.Saddle) + ` up --skip-doctor; echo "[saddle up exited $?]"; exec cat`
	must(w.T, w.Tmux.NewSession(tuiSession, width, height, w.Repo, cmd))
	u := &TUI{w: w, Target: tuiSession + ":0"}
	u.WaitScreen("orchestrator")
	return u
}

// #266, on the real binary without color:
//   - P9: the focused pane's title carries ▶, which moves with tab;
//   - P1: an API error shows once;
//   - P7: a message sent mid-turn shows as queued and is sent when the
//     turn ends, after its reply;
//   - P6: the help overlay lists alt+n/p once and says where / goes;
//   - P10: s opens a spawn prompt that spawns the agent;
//   - P2: at 80x24 the header keeps the orchestrator's state, and at 60x20
//     the chat says how to reach the hidden agents.
func TestJourneyTUIPolish(t *testing.T) {
	w := world(t, Options{})
	u := w.startTUIEnv(140, 40, "NO_COLOR=1")
	u.WaitScreen("▶ ORCHESTRATOR")
	u.Keys("Tab")
	u.WaitScreen("▶ AGENTS")
	u.Keys("Tab")
	u.WaitScreen("▶ ORCHESTRATOR")

	// P1.
	u.Type("fail: 529 overloaded")
	u.Keys("Enter")
	s := u.WaitScreen("Orchestrator error: API Error: 529 overloaded")
	if n := strings.Count(s, "529 overloaded"); n != 2 { // the "you" line and the error
		t.Fatalf("the error shows %d times, want once:\n%s", n-1, s)
	}

	// P7.
	u.Type("slow: long one")
	u.Keys("Enter")
	u.WaitScreen("esc to interrupt")
	u.Type("after the long one")
	u.Keys("Enter")
	u.WaitScreen("you · queued")
	b, _ := os.ReadFile(fakeagent.OrchestratorLog(w.Scripts))
	if strings.Contains(string(b), "after the long one") {
		t.Fatalf("a queued message reached the orchestrator mid-turn:\n%s", b)
	}
	u.Keys("Escape")
	u.WaitScreen("Interrupted.", "fake orchestrator ack: after the long one")
	u.WaitGone("you · queued")
	if s := u.Screen(); strings.Index(s, "Interrupted.") > strings.LastIndex(s, "after the long one") {
		t.Fatalf("the queued message is above the end of the turn before it:\n%s", s)
	}

	// P6: help from the agent list.
	u.Keys("Tab")
	u.Type("?")
	s = u.WaitScreen("KEYS", "alt+n/p", "orchestrator's skills", "run a skill in that agent")
	if strings.Contains(s, "alt+p ") {
		t.Fatalf("help lists alt+p apart from alt+n/p:\n%s", s)
	}
	u.Keys("Escape")
	u.WaitGone("KEYS")

	// P10.
	u.Type("s")
	u.WaitScreen("SPAWN AN AGENT", "spawn ›")
	u.Type("Polish spawn check")
	u.Keys("Enter")
	u.WaitScreen("Spawned t1: Polish spawn check")
	w.WaitTask("t1", "spawned", func(v mcpserver.TaskView) bool { return v.Title == "Polish spawn check" })

	// P2.
	u.Resize(80, 24)
	u.WaitScreen("orch idle")
	u.Resize(60, 20)
	u.WaitScreen("tab: 1 agent")
	u.Quit()
}
