//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/usage"
)

// idleAgent spawns task id with an agent that starts and waits for input
// that never comes, and waits until it is idle at its prompt.
func idleAgent(w *World, id, title, claim string) {
	w.T.Helper()
	w.Spawn(id, title, []string{claim}, fa.Wait("never-comes"))
	w.WaitStatus(id, "idle")
}

// rescript replaces task id's script, so the agent saddle relaunches or
// resumes does something new.
func rescript(w *World, id string, steps ...fa.Step) {
	w.T.Helper()
	must(w.T, fa.Script{Steps: steps}.Save(w.Scripts, id))
}

// ownWindow reports whether task id has a live window named after it.
func ownWindow(w *World, id string) bool {
	for _, win := range w.Tmux.Windows() {
		if strings.HasPrefix(win.Name, id+"-") && !win.Dead {
			return true
		}
	}
	return false
}

// startEngine runs `saddle plugin engine`, which runs saddle up's watchers
// without the TUI, outside the private tmux server so killing the server
// leaves it running. It stops when the test ends.
func startEngine(w *World) {
	w.T.Helper()
	cmd := exec.Command(w.Bins.Saddle, "plugin", "engine")
	cmd.Dir, cmd.Env = w.Repo, w.Env()
	log, err := os.Create(filepath.Join(w.Scripts, "engine.out"))
	must(w.T, err)
	cmd.Stdout, cmd.Stderr = log, log
	must(w.T, cmd.Start())
	w.T.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		_ = log.Close()
	})
}

func queued(v mcpserver.TaskView) bool { return v.Status == "done" && v.Train == "queued" }

// TestJourneyResumeAfterTmuxServerDies (#254): two agents run; the tmux
// server is killed under them. The watcher saddle up runs gives both a new
// window in their worktrees: t1, whose Claude session is on disk, resumes
// it; t2 is relaunched with its brief and a note. Both finish and land, and
// the orchestrator hears about it once.
func TestJourneyResumeAfterTmuxServerDies(t *testing.T) {
	w := world(t, Options{})
	idleAgent(w, "t1", "Alpha work", "alpha/**")
	idleAgent(w, "t2", "Beta work", "beta/**")
	// t1's session (the fake agent reports fake-t1) has a transcript.
	p := usage.ClaudeTranscriptPath(filepath.Join(w.Home, ".claude"), w.Task("t1").Worktree, "fake-t1")
	must(t, os.MkdirAll(filepath.Dir(p), 0o755))
	must(t, os.WriteFile(p, []byte("{}\n"), 0o644))
	startEngine(w)

	rescript(w, "t1", finished("alpha", "alpha\n")...)
	rescript(w, "t2", finished("beta", "beta\n")...)
	if _, err := w.Tmux.Run("kill-server"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"t1", "t2"} {
		Eventually(t, id+"'s window to come back", func() error {
			if !ownWindow(w, id) {
				return errorf("windows: %+v", w.Tmux.Windows())
			}
			return nil
		})
	}
	for _, id := range []string{"t1", "t2"} {
		w.WaitTask(id, "queued in the train", queued)
	}
	w.WaitAgentLog("t1", "prompt: [saddle] saddle restarted")
	w.WaitAgentLog("t2", "may already exist in the worktree")
	b, err := os.ReadFile(filepath.Join(w.Repo, ".saddle", "run", "t1", "launch.sh"))
	must(t, err)
	if !strings.Contains(string(b), "--resume 'fake-t1'") {
		t.Fatalf("t1 was not resumed with its session:\n%s", b)
	}
	r := w.MustSaddle("land")
	for _, id := range []string{"t1", "t2"} {
		if w.Task(id).Status != "landed" {
			t.Fatalf("%s didn't land: %s", id, r)
		}
	}
	r = w.MustSaddle("notices", "--all")
	if n := strings.Count(r.Stdout, "Resumed 2 tasks"); n != 1 {
		t.Fatalf("orchestrator heard about the resume %d times:\n%s", n, r.Stdout)
	}
}

// TestJourneyDownThenUpResumes (#254): saddle down pauses the running task,
// keeping its claims, and says so; saddle up resumes it, and it finishes.
func TestJourneyDownThenUpResumes(t *testing.T) {
	w := world(t, Options{})
	idleAgent(w, "t1", "Alpha work", "alpha/**")

	r := w.MustSaddle("down")
	if !strings.Contains(r.Stdout, "paused 1 agents: t1") {
		t.Fatalf("down: %s", r)
	}
	if v := w.Task("t1"); v.Status != "paused" || len(v.Claims) != 1 {
		t.Fatalf("after down: %+v", v)
	}
	if ownWindow(w, "t1") {
		t.Fatal("t1 still has a window after down")
	}

	rescript(w, "t1", finished("alpha", "alpha\n")...)
	u := w.StartTUI(160, 45)
	w.WaitTask("t1", "queued in the train", queued)
	u.Quit()
	w.MustSaddle("land")
	w.WaitStatus("t1", "landed")
}

// TestJourneyRescueRestoresUncommitted (#254): an agent's uncommitted work
// survives losing its window: saddle rescue saves it to rescue/<task> and a
// patch, kills the task and frees its claims, and the patch restores it.
func TestJourneyRescueRestoresUncommitted(t *testing.T) {
	w := world(t, Options{})
	w.Spawn("t1", "Alpha work", []string{"alpha/**"}, fa.Write("alpha/wip.txt", "half done\n"), fa.Wait("never-comes"))
	w.WaitStatus("t1", "idle")
	wt := w.Task("t1").Worktree
	if _, err := w.Tmux.Run("kill-server"); err != nil {
		t.Fatal(err)
	}

	r := w.MustSaddle("rescue", "t1")
	if !strings.Contains(r.Stdout, "rescue/t1") {
		t.Fatalf("rescue: %s", r)
	}
	if got := w.Git(w.Repo, "show", "rescue/t1:alpha/wip.txt"); got != "half done" {
		t.Fatalf("rescue/t1:alpha/wip.txt = %q", got)
	}
	if v := w.Task("t1"); v.Status != "killed" || len(v.Claims) != 0 {
		t.Fatalf("after rescue: %+v", v)
	}
	must(t, os.Remove(filepath.Join(wt, "alpha", "wip.txt")))
	w.Git(wt, "apply", filepath.Join(w.Repo, ".saddle", "rescue", "t1.diff"))
	if b, _ := os.ReadFile(filepath.Join(wt, "alpha", "wip.txt")); string(b) != "half done\n" {
		t.Fatalf("restored wip.txt = %q", b)
	}
	// The claim is free again.
	w.Spawn("t2", "Alpha again", []string{"alpha/**"}, fa.Wait("never-comes"))
}
