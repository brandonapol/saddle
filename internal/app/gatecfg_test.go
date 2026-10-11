package app

import (
	"fmt"
	"strings"
	"testing"
)

// #339: lint.cmd edited in .saddle/config.toml between two done calls must
// take effect on the second, without restarting saddle; the refusal names
// the command it ran.
func TestDoneReloadsGateCmdBetweenDones(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	tk, err := a.Spawn(SpawnReq{Title: "gate"})
	must(t, err)
	write(t, tk.Worktree, "x.go", "ok\n")
	commitAll(t, tk.Worktree, "work")

	write(t, a.Root, ".saddle/config.toml", "[test]\ncmd = \"none\"\n[train]\nlint.cmd = 'echo old gate; exit 1'\n")
	err = a.Done(tk.ID, "s")
	if err == nil || !strings.Contains(err.Error(), "echo old gate; exit 1") {
		t.Fatalf("first done, with a red gate configured after Open: %v", err)
	}

	write(t, a.Root, ".saddle/config.toml", "[test]\ncmd = \"none\"\n[train]\nlint.cmd = 'true'\n")
	must(t, a.Done(tk.ID, "s"))
	if !inTrain(t, a, tk.ID) {
		t.Fatal("second done, after lint.cmd changed to a green gate, did not queue the task")
	}
}

// DoneAsync, the MCP done tool's path, reloads the gate too.
func TestDoneAsyncReloadsGateCmd(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	tk, err := a.Spawn(SpawnReq{Title: "gate"})
	must(t, err)
	write(t, tk.Worktree, "x.go", "ok\n")
	commitAll(t, tk.Worktree, "work")

	write(t, a.Root, ".saddle/config.toml", "[test]\ncmd = \"none\"\n[train]\nlint.cmd = 'echo new gate; exit 1'\n")
	_, err = a.DoneAsync(tk.ID, "s")
	a.WaitDoneJobs()
	if inTrain(t, a, tk.ID) {
		t.Fatal("DoneAsync queued the task past the red gate configured after Open")
	}
	// Refused inline or in the background job's notice; either names the gate.
	if got := fmt.Sprint(err) + noticeTexts(t, a, tk.ID); !strings.Contains(got, "echo new gate") {
		t.Fatalf("no refusal naming the configured gate: %s", got)
	}
}
