package cli

import (
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/store"
)

func TestRootRegistersResumeAndRescue(t *testing.T) {
	for _, name := range []string{"resume", "rescue"} {
		if c, _, err := Root().Find([]string{name}); err != nil || c.Name() != name {
			t.Fatalf("saddle %s not registered: %v", name, err)
		}
	}
}

// #254: SIGTERM on saddle up pauses running tasks; without it nothing changes.
func TestPauseOnTerm(t *testing.T) {
	a := automergeRepo(t) // t1 is running
	term := make(chan os.Signal, 1)
	if p, err := pauseOnTerm(term, a); p != nil || err != nil {
		t.Fatalf("no signal: paused %v, %v", p, err)
	}
	if tk, _ := a.Store.Task("t1"); tk.Status != store.Running {
		t.Fatalf("t1 is %s without a signal", tk.Status)
	}
	term <- syscall.SIGTERM
	p, err := pauseOnTerm(term, a)
	if err != nil || len(p) != 1 || p[0] != "t1" {
		t.Fatalf("SIGTERM paused %v, %v", p, err)
	}
	if tk, _ := a.Store.Task("t1"); tk.Status != app.StatusPaused {
		t.Fatalf("t1 is %s after SIGTERM", tk.Status)
	}
	var out strings.Builder
	writePaused(&out, p)
	if !strings.Contains(out.String(), "paused 1 agents: t1") {
		t.Fatalf("output: %s", out.String())
	}
}

// saddle rescue saves uncommitted work and kills the task; saddle resume
// refuses a dead task.
func TestRescueCommand(t *testing.T) {
	a := automergeRepo(t)
	tk, _ := a.Store.Task("t1")
	if err := os.WriteFile(tk.Worktree+"/wip.txt", []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, rescueCmd(), "t1")
	if err != nil || !strings.Contains(out, "rescue/t1") || !strings.Contains(out, "t1.diff") {
		t.Fatalf("rescue: %v\n%s", err, out)
	}
	if got := gitRun(t, a.Root, "show", "rescue/t1:wip.txt"); strings.TrimSpace(got) != "wip" {
		t.Fatalf("rescue/t1:wip.txt = %q", got)
	}
	if out, err := runCmd(t, resumeCmd(), "t1"); err == nil {
		t.Fatalf("resumed a killed task:\n%s", out)
	}
}
