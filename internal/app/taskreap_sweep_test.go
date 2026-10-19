package app

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/procreap"
	"github.com/brandonapol/saddle/internal/store"
)

// taskOrphan leaks a process the way an agent does (#690): sh backgrounds a
// bare sleep and exits, so it is reparented, in an unrelated cwd, tagged
// only by its environment.
func taskOrphan(t *testing.T, a *App, task string) int {
	t.Helper()
	c := exec.Command("sh", "-c", "sleep 600 </dev/null >/dev/null 2>&1 & echo $!")
	c.Dir = t.TempDir()
	c.Env = append(os.Environ(), procreap.EnvRoot+"="+a.Root, procreap.EnvTask+"="+task)
	out, err := c.StdoutPipe()
	must(t, err)
	must(t, c.Start())
	line, _ := bufio.NewReader(out).ReadString('\n')
	_ = c.Wait()
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	must(t, err)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

func waitDead(pid int) bool {
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil || strings.Contains(string(b), ") Z ") {
			return true
		}
	}
	return false
}

// #690: a task that ends no_change must not leave a tagged process running.
func TestNoChangeReapsTaggedProcesses(t *testing.T) {
	needProc(t)
	a, _ := setup(t)
	task, err := a.Spawn(SpawnReq{Title: "leak"})
	must(t, err)
	pid := taskOrphan(t, a, task.ID)
	must(t, a.finishNoChange(task, "", "already fixed"))
	if !waitDead(pid) {
		t.Error("no_change left the tagged orphan running")
	}
}

// #690: done reaps too, but a handoff does not (the successor reuses the task).
func TestDoneReapsTaggedProcesses(t *testing.T) {
	needProc(t)
	a, _ := setup(t)
	task, err := a.Spawn(SpawnReq{Title: "leak"})
	must(t, err)
	pid := taskOrphan(t, a, task.ID)
	must(t, a.finishDone(task, "s"))
	if !waitDead(pid) {
		t.Error("done left the tagged orphan running")
	}
}

// #690: kill reaps by tag with no run dir to match.
func TestKillReapsTaggedProcesses(t *testing.T) {
	needProc(t)
	a, _ := setup(t)
	task, err := a.Spawn(SpawnReq{Title: "leak"})
	must(t, err)
	pid := taskOrphan(t, a, task.ID)
	must(t, a.Kill(task.ID, true))
	if !waitDead(pid) {
		t.Error("kill left the tagged orphan running")
	}
}

// #690: the sweep kills processes whose task is already terminal (a leak
// from before the fix), reports them, and spares live tasks' and unknown ones.
func TestSweepOrphanTasks(t *testing.T) {
	needProc(t)
	a, _ := setup(t)
	dead, err := a.Spawn(SpawnReq{Title: "dead"})
	must(t, err)
	live, err := a.Spawn(SpawnReq{Title: "live"})
	must(t, err)
	must(t, a.Store.SetStatus(dead.ID, store.NoChange))
	leak := taskOrphan(t, a, dead.ID)
	keep := taskOrphan(t, a, live.ID)
	unknown := taskOrphan(t, a, "t9999")

	if got := a.OrphanTaskProcs(); len(got[dead.ID]) != 1 || len(got[live.ID]) != 0 || len(got["t9999"]) != 0 {
		t.Fatalf("OrphanTaskProcs = %v", got)
	}
	res := a.SweepOrphanTasks()
	if res.Killed < 1 || len(res.Tasks) != 1 || res.Tasks[0] != dead.ID {
		t.Fatalf("sweep = %+v", res)
	}
	if !waitDead(leak) {
		t.Error("the orphan of a no_change task survived the sweep")
	}
	for _, p := range []int{keep, unknown} {
		if b, err := os.ReadFile("/proc/" + strconv.Itoa(p) + "/stat"); err != nil || strings.Contains(string(b), ") Z ") {
			t.Errorf("pid %d was killed", p)
		}
	}
}
