//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
	"github.com/brandonapol/saddle/internal/mcpserver"
)

// TestJourneyTwoAgentsDoneTogether (#271): two agents commit and call done at
// the same moment. The repo's gate refuses to start while another copy of it
// runs (as golangci-lint does), so done's gate must wait its turn on the
// repo-hook lock instead of running beside the other: no false refusal, both
// branches reach the train and land.
func TestJourneyTwoAgentsDoneTogether(t *testing.T) {
	busy := filepath.Join(t.TempDir(), "gate-busy")
	runs := filepath.Join(filepath.Dir(busy), "gate-runs")
	gate := "lint:\n\t@if ! mkdir " + shq(busy) + " 2>/dev/null; then echo 'gate: another run is active'; exit 1; fi; " +
		"echo run >> " + shq(runs) + "; sleep 2; rmdir " + shq(busy) + "\n"
	w := world(t, Options{Tables: "[train]\nno_auto_rebase = true\n"})
	w.WriteFile("Makefile", gate)
	w.Git(w.Repo, "add", "Makefile")
	w.Git(w.Repo, "commit", "-q", "-m", "gate")
	w.Git(w.Repo, "push", "-q", "origin", "main")

	// Both agents gate on a message so their done calls fire together.
	for _, id := range []string{"t1", "t2"} {
		dir := "dir" + id
		w.Spawn(id, "Work "+id, []string{dir + "/**"},
			fa.Write(dir+"/work.txt", id+"\n"), fa.Commit("work "+id),
			fa.Wait("go-now"), fa.Done("Adds "+dir+"."))
	}
	w.MustSaddle("message", "t1", "go-now")
	w.MustSaddle("message", "t2", "go-now")

	for _, id := range []string{"t1", "t2"} {
		w.WaitTask(id, "queued in the train", func(v mcpserver.TaskView) bool { return v.Status == "done" && v.Train == "queued" })
		if log := w.AgentLog(id); strings.Contains(log, "done refused") || strings.Contains(log, "another run is active") {
			t.Fatalf("%s was refused although its gate only had to wait:\n%s", id, log)
		}
	}
	b, _ := os.ReadFile(runs)
	if n := strings.Count(string(b), "run"); n != 2 {
		t.Fatalf("the gate ran %d times, want once per agent", n)
	}
	w.MustSaddle("land")
	for _, id := range []string{"t1", "t2"} {
		w.WaitStatus(id, "landed")
	}
}
