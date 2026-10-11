package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/refguard"
	"github.com/brandonapol/saddle/internal/store"
)

// exclusiveGate is a gate command that logs each run and, when another run
// is active at the same moment, logs "overlap" and fails, as golangci-lint
// refuses to run beside itself. Its message isn't one ClassifyGateOutput
// knows, so done can't retry its way past an overlap.
func exclusiveGate(t *testing.T) (cmd string, runs func() int, overlaps func() int) {
	t.Helper()
	dir := t.TempDir()
	busy, log := shellQuote(filepath.Join(dir, "busy")), shellQuote(filepath.Join(dir, "log"))
	cmd = "if ! mkdir " + busy + " 2>/dev/null; then echo overlap >> " + log + "; echo 'gate: another run is active'; exit 1; fi; " +
		"echo run >> " + log + "; sleep 1; rmdir " + busy
	count := func(word string) int {
		b, _ := os.ReadFile(filepath.Join(dir, "log"))
		return strings.Count(string(b), word)
	}
	return cmd, func() int { return count("run") }, func() int { return count("overlap") }
}

// #271: two agents' done gates started together run one after the other on
// the repo hooks' lock: neither is refused for the other's run.
func TestLintDoneGatesRunOneAtATime(t *testing.T) {
	a := trainSetup(t)
	cmd, runs, overlaps := exclusiveGate(t)
	a.Cfg.Train.Lint = config.Lint{Cmd: cmd, Set: true}
	var tasks []store.Task
	for _, title := range []string{"one", "two"} {
		tk, err := a.Spawn(SpawnReq{Title: title})
		must(t, err)
		write(t, tk.Worktree, title+".txt", title+"\n")
		commitAll(t, tk.Worktree, title)
		tasks = append(tasks, tk)
	}
	errs := make([]error, len(tasks))
	var wg sync.WaitGroup
	for i, tk := range tasks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = a.lintDone(tk)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("%s's done was refused: %v", tasks[i].ID, err)
		}
	}
	if n := overlaps(); n != 0 {
		t.Fatalf("the two done gates ran at once %d times", n)
	}
	if n := runs(); n != 2 {
		t.Fatalf("gate ran %d times, want once per task", n)
	}
}

// #271, the quark incident: one agent's commit runs the repo's pre-commit
// hook (saddle's wrapper, under its flock) while another agent's done gate
// starts. The done gate waits for the hook instead of running beside it,
// and the agent whose commit the hook just passed is not checked again.
func TestLintDoneWaitsForTheRepoHookAndHonoursItsStamp(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not installed")
	}
	a := trainSetup(t)
	cmd, runs, overlaps := exclusiveGate(t)
	write(t, a.Root, "Makefile", "lint:\n\t@"+cmd+"\n")
	write(t, a.Root, "git/hooks/pre-commit", "#!/bin/sh\nmake lint\n")
	must(t, os.Chmod(filepath.Join(a.Root, "git/hooks/pre-commit"), 0o755))
	commitAll(t, a.Root, "repo gate")
	if _, err := refguard.InstallRepoHooks(a.Root); err != nil {
		t.Fatal(err)
	}
	if g := a.LintGate(); g.Cmd != "make lint" {
		t.Fatalf("gate = %+v", g)
	}
	committer, err := a.Spawn(SpawnReq{Title: "committer"})
	must(t, err)
	doer, err := a.Spawn(SpawnReq{Title: "doer"})
	must(t, err)
	// The doer committed earlier with hooks off: its tree has no stamp.
	write(t, doer.Worktree, "doer.txt", "doer\n")
	git(t, doer.Worktree, "add", "-A")
	git(t, doer.Worktree, "-c", "core.hooksPath=/dev/null", "commit", "-qm", "doer")
	write(t, committer.Worktree, "committer.txt", "committer\n")
	git(t, committer.Worktree, "add", "-A")

	var wg sync.WaitGroup
	var commitOut []byte
	var commitErr, doneErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		c := exec.Command("git", "-C", committer.Worktree, "commit", "-qm", "committer")
		c.Env = append(os.Environ(), "SADDLE_TASK="+committer.ID, "SADDLE_TRAIN=", "SADDLE_FAST_HOOK=")
		commitOut, commitErr = c.CombinedOutput()
	}()
	go func() {
		defer wg.Done()
		doneErr = a.lintDone(doer)
	}()
	wg.Wait()
	if commitErr != nil {
		t.Fatalf("the committer's hook failed: %v\n%s", commitErr, commitOut)
	}
	if doneErr != nil {
		t.Fatalf("the doer's done was refused: %v", doneErr)
	}
	if n := overlaps(); n != 0 {
		t.Fatalf("done's gate ran beside the repo hook %d times", n)
	}
	if n := runs(); n != 2 {
		t.Fatalf("gate ran %d times, want the hook once and the doer's done once", n)
	}
	must(t, a.lintDone(committer))
	if n := runs(); n != 2 {
		t.Fatalf("done re-ran the gate on the tree the hook just passed (%d runs)", n)
	}
}
