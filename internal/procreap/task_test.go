package procreap

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// orphan starts a process the way a task agent leaks one (#690): sh forks a
// bare `sleep` into the background and exits, so the sleep is reparented,
// keeps an unrelated cwd, and has an argv that names nothing of saddle's.
func orphan(t *testing.T, dir string, env []string) int {
	t.Helper()
	c := exec.Command("sh", "-c", "sleep 60 </dev/null >/dev/null 2>&1 & echo $!")
	c.Dir = dir
	c.Env = append(os.Environ(), env...)
	out, err := c.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(out).ReadString('\n')
	_ = c.Wait()
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("no pid from %q: %v", line, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

func alive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	return err == nil && !strings.Contains(string(b), ") Z ")
}

func tag(root, task string) []string {
	return []string{EnvRoot + "=" + root, EnvTask + "=" + task}
}

// #690: a task's leaked process is found by its SADDLE_ROOT + SADDLE_TASK
// tag alone, whatever its cwd or argv; another task's, another repo's and an
// untagged one are spared.
func TestReapTaskKillsOnlyThatTasksTaggedProcesses(t *testing.T) {
	needProc(t)
	root, elsewhere := t.TempDir(), t.TempDir()
	mine := orphan(t, elsewhere, tag(root, "t5"))
	sibling := orphan(t, elsewhere, tag(root, "t6"))
	foreign := orphan(t, elsewhere, tag(elsewhere, "t5"))
	plain := orphan(t, elsewhere, nil)

	got := FindTask(root, "t5")
	if len(got) != 1 || got[0] != mine {
		t.Fatalf("FindTask = %v, want [%d]", got, mine)
	}
	if n := ReapTask(root, "t5", time.Second); n != 1 {
		t.Fatalf("ReapTask signalled %d, want 1", n)
	}
	if alive(mine) {
		t.Error("the tagged orphan survived")
	}
	for _, p := range []int{sibling, foreign, plain} {
		if !alive(p) {
			t.Errorf("pid %d was killed; it is not t5's", p)
		}
	}
}

// #690: Tagged groups every tagged process of a root by task, for the orphan
// sweep.
func TestTaggedGroupsByTask(t *testing.T) {
	needProc(t)
	root, elsewhere := t.TempDir(), t.TempDir()
	a := orphan(t, elsewhere, tag(root, "t1"))
	b := orphan(t, elsewhere, tag(root, "t2"))
	orphan(t, elsewhere, tag(elsewhere, "t3"))
	m := Tagged(root)
	if len(m["t1"]) != 1 || m["t1"][0] != a || len(m["t2"]) != 1 || m["t2"][0] != b || len(m["t3"]) != 0 {
		t.Fatalf("Tagged = %v", m)
	}
}
