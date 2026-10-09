//go:build e2e

package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
	"github.com/brandonapol/saddle/internal/mcpserver"
)

// These journeys re-state #236's acceptance criteria through the real saddle
// binary (#245): fake agents in tmux panes whose Bash calls go through the
// PreToolUse rewrite and the PATH shims, saddle sessions sharing one
// XDG_STATE_HOME, and the train's gate. The heavy tools are fakes that log
// to files, and every wait is on a condition.

// runqWorld is a World whose heavy-run queue enforces with a 1s heartbeat
// and the user runq.toml extra, with env added to everything it runs (the
// panes too: the tmux server starts with the first spawn).
func runqWorld(t *testing.T, opts Options, extra string, env ...string) *World {
	t.Helper()
	w := world(t, opts)
	w.env = append(w.env, env...)
	w.Tmux.Env = w.env
	must(t, os.MkdirAll(filepath.Join(w.Home, ".config", "saddle"), 0o755))
	must(t, os.WriteFile(filepath.Join(w.Home, ".config", "saddle", "runq.toml"),
		[]byte("mode = \"enforce\"\nheartbeat = \"1s\"\n"+extra), 0o644))
	return w
}

// fakeFlutter puts a flutter on the World's PATH that, for `flutter test`,
// logs "start <task>" to dir/log and its pid to dir/<task>.pid, waits for
// dir/release-<task> (spinning a CPU when burn is set), then logs "end
// <task>". It must exist before the first spawn, which writes the shims.
func fakeFlutter(t *testing.T, w *World, dir string, burn bool) {
	t.Helper()
	wait := "sleep 0.02"
	if burn {
		wait = ":"
	}
	d := shq(dir)
	must(t, os.WriteFile(filepath.Join(w.Bin, "flutter"), []byte(`#!/bin/sh
echo "start $SADDLE_TASK lease=$SADDLE_RUNQ_LEASE" >> `+d+`/log
mkdir `+d+`/running/$SADDLE_TASK
echo $$ > `+d+`/$SADDLE_TASK.pid.tmp && mv `+d+`/$SADDLE_TASK.pid.tmp `+d+`/$SADDLE_TASK.pid
while [ ! -e `+d+`/release-$SADDLE_TASK ]; do `+wait+`; done
rmdir `+d+`/running/$SADDLE_TASK
echo "end $SADDLE_TASK" >> `+d+`/log
`), 0o755))
	must(t, os.MkdirAll(filepath.Join(dir, "running"), 0o755))
}

// heavyRuns is the heavy-run queue as `saddle status --json` shows it.
func (w *World) heavyRuns(t *testing.T) *app.HeavyRuns {
	t.Helper()
	return heavyRunsOf(t, w.MustSaddle("status", "--json").Stdout)
}

// heavyClass is class in v, or a zero HeavyClass.
func heavyClass(v *app.HeavyRuns, class string) app.HeavyClass {
	if v != nil {
		for _, c := range v.Classes {
			if c.Class == class {
				return c
			}
		}
	}
	return app.HeavyClass{}
}

// waitHeavy waits until class's holders and waiters, by task, are holders
// and waiters (in grant order).
func (w *World) waitHeavy(t *testing.T, class string, holders, waiters []string) app.HeavyClass {
	t.Helper()
	var c app.HeavyClass
	Eventually(t, class+" holders "+strings.Join(holders, ",")+" waiters "+strings.Join(waiters, ","), func() error {
		c = heavyClass(w.heavyRuns(t), class)
		var hs, ws []string
		for _, h := range c.Holders {
			hs = append(hs, h.Task)
		}
		for _, wt := range c.Waiters {
			ws = append(ws, wt.Task)
		}
		if strings.Join(hs, ",") != strings.Join(holders, ",") || strings.Join(ws, ",") != strings.Join(waiters, ",") {
			return errorf("%s: holders %v waiters %v", class, hs, ws)
		}
		return nil
	})
	return c
}

func release(t *testing.T, dir, task string) {
	t.Helper()
	must(t, os.WriteFile(filepath.Join(dir, "release-"+task), nil, 0o644))
}

// logLines is dir/log's lines with the lease dropped.
func logLines(dir string) string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(readFile(filepath.Join(dir, "log"))), "\n") {
		l, _, _ = strings.Cut(l, " lease=")
		out = append(out, l)
	}
	return strings.Join(out, " | ")
}

// TestJourneyRunqTwoSessionsTakeTurns: two saddle sessions on two repos
// share one XDG_STATE_HOME. Each one's agent runs `flutter test`, which the
// PreToolUse hook rewrites to `saddle run`. The second agent's run queues
// at position 1 behind the first session's and starts by itself when it
// ends.
func TestJourneyRunqTwoSessionsTakeTurns(t *testing.T) {
	state, dir := t.TempDir(), t.TempDir()
	cfg := "max_load_per_cpu = 0\nmax_cpu_pressure = 0\n[classes.flutter-test]\nslots = 1\n"
	a := runqWorld(t, Options{}, cfg, "XDG_STATE_HOME="+state)
	fakeFlutter(t, a, dir, false)
	b := runqWorld(t, Options{}, cfg, "XDG_STATE_HOME="+state)
	fakeFlutter(t, b, dir, false)
	release(t, dir, "t2")

	a.Spawn("t1", "Flutter in A", []string{"lib/**"}, fa.Run("flutter test"))
	Eventually(t, "t1's flutter test running", fileHas(filepath.Join(dir, "log"), "start t1"))
	b.Spawn("t2", "Flutter in B", []string{"lib/**"}, fa.Run("flutter test"))
	c := b.waitHeavy(t, "flutter-test", []string{"t1"}, []string{"t2"})
	if c.Slots != 1 || c.Waiters[0].Position != 1 {
		t.Fatalf("B's view of the queue: %+v", c)
	}
	if a := heavyClass(a.heavyRuns(t), "flutter-test"); len(a.Waiters) != 1 || a.Waiters[0].Task != "t2" {
		t.Fatalf("A doesn't see B's waiter: %+v", a)
	}
	b.WaitAgentLog("t2", "rewritten: "+b.Bins.Saddle+" run --class flutter-test --prio worker -- flutter test")
	if strings.Contains(readFile(filepath.Join(dir, "log")), "start t2") {
		t.Fatalf("t2 ran while t1 held the only slot:\n%s", logLines(dir))
	}

	release(t, dir, "t1")
	b.WaitAgentLog("t2", "step: 1 run ok")
	a.WaitAgentLog("t1", "step: 1 run ok")
	if got := logLines(dir); got != "start t1 | end t1 | start t2 | end t2" {
		t.Fatalf("runs overlapped or misordered: %s", got)
	}
	out := b.AgentLog("t2")
	if !strings.Contains(out, "queued: position 1 of 1 for flutter-test, holder ") || !strings.Contains(out, "runq: flutter-test slot acquired after") {
		t.Fatalf("t2 didn't report its wait:\n%s", out)
	}
}

// panePIDs is every process in task's pane: its pane process and all its
// descendants.
func (w *World) panePIDs(t *testing.T, task string) []int {
	t.Helper()
	win := w.Task(task).Window
	out, err := w.Tmux.Run("display-message", "-p", "-t", win, "#{pane_pid}")
	if err != nil || win == "" {
		t.Fatalf("pane of %s (window %q): %v", task, win, err)
	}
	root, err := strconv.Atoi(strings.TrimSpace(out))
	must(t, err)
	pids := []int{root}
	for i := 0; i < len(pids); i++ {
		kids, _ := exec.Command("pgrep", "-P", strconv.Itoa(pids[i])).Output()
		for _, f := range strings.Fields(string(kids)) {
			if n, err := strconv.Atoi(f); err == nil {
				pids = append(pids, n)
			}
		}
	}
	return pids
}

func readPID(t *testing.T, p string) int {
	t.Helper()
	n, err := strconv.Atoi(strings.TrimSpace(readFile(p)))
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return n
}

// TestJourneyRunqCrashedHolderFreesSlot: the agent holding the only
// flutter-test slot crashes: its whole pane (the agent, its shell and its
// saddle run) is SIGKILLed with no chance to clean up. Only its flutter
// child is spared, so nothing but the lease's death can stop it. The
// waiting agent starts within about one heartbeat (1s), and the dead
// holder's child is gone.
func TestJourneyRunqCrashedHolderFreesSlot(t *testing.T) {
	dir := t.TempDir()
	w := runqWorld(t, Options{}, "max_load_per_cpu = 0\nmax_cpu_pressure = 0\n[classes.flutter-test]\nslots = 1\n")
	fakeFlutter(t, w, dir, false)
	release(t, dir, "t2")
	w.Spawn("t1", "Flutter one", []string{"one/**"}, fa.Run("flutter test"))
	pidFile := filepath.Join(dir, "t1.pid")
	Eventually(t, "t1's flutter test running", func() error {
		_, err := os.Stat(pidFile)
		return err
	})
	child := readPID(t, pidFile)
	w.Spawn("t2", "Flutter two", []string{"two/**"}, fa.Run("flutter test"))
	w.waitHeavy(t, "flutter-test", []string{"t1"}, []string{"t2"})

	var crashed []int
	for _, pid := range w.panePIDs(t, "t1") {
		if pid != child {
			crashed = append(crashed, pid)
		}
	}
	if len(crashed) < 3 { // the agent, its sh -c, saddle run
		t.Fatalf("t1's pane has too few processes to be holding the lease: %v (child %d)", crashed, child)
	}
	killed := time.Now()
	for _, pid := range crashed {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	Eventually(t, "t2's flutter test started", fileHas(filepath.Join(dir, "log"), "start t2"))
	if took := time.Since(killed); took > 2*time.Second {
		t.Fatalf("t2 started %s after the holder died; want about one heartbeat (1s)", took)
	}
	Eventually(t, "the dead holder's flutter to die too", func() error {
		if err := syscall.Kill(child, 0); err == nil {
			return errorf("pid %d still alive", child)
		}
		return nil
	})
	w.WaitAgentLog("t2", "step: 1 run ok")
	if strings.Contains(readFile(filepath.Join(dir, "log")), "end t1") {
		t.Fatal("the dead holder's flutter finished instead of being killed")
	}
}

// TestJourneyRunqHookReentrancy: the train's gate for t1 holds the only
// go-test slot. Its test.cmd commits, and the pre-commit hook's `make check`
// goes through the make shim on the gate's lease instead of queueing behind
// it. Meanwhile agent t2 commits: the same hook's `make check` queues behind
// the gate. Nothing deadlocks: the land finishes, then t2's check runs on a
// lease of its own and its commit goes through.
func TestJourneyRunqHookReentrancy(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not installed")
	}
	dir := t.TempDir()
	gate := filepath.Join(dir, "gate.sh")
	w := runqWorld(t, Options{TestCmd: "sh " + gate},
		"max_load_per_cpu = 0\nmax_cpu_pressure = 0\n[classes.go-test]\nslots = 1\nmatch = [\"sh *gate.sh\", \"make check\"]\n")
	log, hold, holding, rel := filepath.Join(dir, "log"), filepath.Join(dir, "hold"), filepath.Join(dir, "holding"), filepath.Join(dir, "release")
	// The repo's check, run from its own directory so the pattern "make
	// check" matches the hook's argv one for one.
	checkDir := filepath.Join(dir, "check")
	must(t, os.MkdirAll(checkDir, 0o755))
	must(t, os.WriteFile(filepath.Join(checkDir, "Makefile"),
		[]byte("check:\n\t@echo \"check $${SADDLE_TASK:-gate} lease=$$SADDLE_RUNQ_LEASE\" >> "+log+"\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(w.Repo, ".git", "hooks", "pre-commit"),
		[]byte("#!/bin/sh\ncd "+shq(checkDir)+" && exec make check\n"), 0o755))
	// The gate commits in a scratch repo with the repo's hooks and the
	// panes' shims, then, once the test asks, holds its slot until released.
	must(t, os.WriteFile(gate, []byte("set -e\n"+
		"export PATH="+shq(filepath.Join(w.Repo, ".saddle", "shims"))+":$PATH\n"+
		"d=$(mktemp -d)\n"+
		"git init -q \"$d\"\n"+
		"SADDLE_TASK= git -C \"$d\" -c core.hooksPath="+shq(filepath.Join(w.Repo, ".git", "hooks"))+" commit -q --allow-empty -m gate\n"+
		"rm -rf \"$d\"\n"+
		"echo \"gate lease=$SADDLE_RUNQ_LEASE\" >> "+shq(log)+"\n"+
		"if [ -e "+shq(hold)+" ]; then touch "+shq(holding)+"; while [ ! -e "+shq(rel)+" ]; do sleep 0.02; done; fi\n"), 0o644))

	w.Spawn("t1", "Alpha work", []string{"alpha/**"}, finished("alpha", "alpha\n")...)
	w.WaitTask("t1", "queued in the train", func(v mcpserver.TaskView) bool { return v.Status == "done" && v.Train == "queued" })
	_ = os.Remove(log) // t1's own commit and done ran the check and the gate already
	must(t, os.WriteFile(hold, nil, 0o644))

	land := exec.Command(w.Bins.Saddle, "land")
	land.Env, land.Dir = w.Env(), w.Repo
	var landOut bytes.Buffer
	land.Stdout, land.Stderr = &landOut, &landOut
	must(t, land.Start())
	landed := make(chan error, 1)
	go func() { landed <- land.Wait() }()
	t.Cleanup(func() { _ = land.Process.Kill() })
	Eventually(t, "the gate holding go-test", func() error {
		_, err := os.Stat(holding)
		return err
	})

	w.Spawn("t2", "Beta work", []string{"beta/**"}, fa.Write("beta/b.txt", "b\n"), fa.Commit("add b"))
	c := w.waitHeavy(t, "go-test", []string{"t1"}, []string{"t2"}) // the gate's lease is t1's
	if c.Waiters[0].Position != 1 || !strings.Contains(c.Waiters[0].Cmd, "make check") {
		t.Fatalf("t2's hook isn't queued behind the gate: %+v", c)
	}
	select {
	case err := <-landed:
		t.Fatalf("land finished while the gate was held (%v):\n%s", err, landOut.String())
	default:
	}

	must(t, os.WriteFile(rel, nil, 0o644))
	select {
	case err := <-landed:
		if err != nil {
			t.Fatalf("land: %v\n%s", err, landOut.String())
		}
	case <-time.After(Timeout()):
		t.Fatalf("land didn't finish: deadlock?\n%s\nlog:\n%s", landOut.String(), readFile(log))
	}
	if v := w.Task("t1"); v.Status != "landed" {
		t.Fatalf("t1 didn't land: %+v\n%s", v, landOut.String())
	}
	w.WaitAgentLog("t2", "step: 2 commit ok")

	leases := map[string]string{}
	var order []string
	for _, l := range strings.Split(strings.TrimSpace(readFile(log)), "\n") {
		who, lease, _ := strings.Cut(l, " lease=")
		leases[who] = lease
		order = append(order, who)
	}
	if strings.Join(order, ",") != "check gate,gate,check t2" {
		t.Fatalf("check log order %v:\n%s", order, readFile(log))
	}
	if g := leases["gate"]; g == "" || leases["check gate"] != g {
		t.Fatalf("the gate's hook didn't ride its lease:\n%s", readFile(log))
	}
	if l := leases["check t2"]; l == "" || l == leases["gate"] {
		t.Fatalf("t2's check should have its own lease:\n%s", readFile(log))
	}
}

// TestJourneyRunqLoadCeiling: six agents run a CPU-burning `flutter test`
// under a ceiling of two flutter-test slots, each burner one CPU. The load
// source is a fake /proc, so the box's real load can't change the outcome.
// While it reads saturated nobody starts, even with slots free. Once it
// reads idle, the runs go two at a time: the sampled count of burners
// never exceeds the ceiling, and all six finish.
func TestJourneyRunqLoadCeiling(t *testing.T) {
	const ceiling, agents = 2, 6
	dir, proc := t.TempDir(), t.TempDir()
	must(t, os.MkdirAll(filepath.Join(proc, "proc", "pressure"), 0o755))
	loadavg := filepath.Join(proc, "proc", "loadavg")
	must(t, os.WriteFile(loadavg, []byte("999.00 1.00 1.00 2/300 1\n"), 0o644))
	w := runqWorld(t, Options{},
		"max_load_per_cpu = 1.0\nmax_cpu_pressure = 0\ngate_max_wait = \"10m\"\n[classes.flutter-test]\nslots = "+itoa(ceiling)+"\n",
		"SADDLE_RUNQ_PROC="+proc)
	fakeFlutter(t, w, dir, true)
	var tasks []string
	for i := 1; i <= agents; i++ {
		id := "t" + itoa(i)
		tasks = append(tasks, id)
		// With the load gate holding, the waiters back the queue up past
		// spawn's backpressure threshold (#243), which isn't on trial here.
		must(t, fa.Script{Steps: []fa.Step{fa.Run("flutter test")}}.Save(w.Scripts, id))
		w.MustSaddle("spawn", "--force", "--id", id, "-c", "dir"+itoa(i)+"/**", "Burner "+id, "Burn")
	}
	running := func() int {
		ents, _ := os.ReadDir(filepath.Join(dir, "running"))
		return len(ents)
	}
	Eventually(t, "all six waiting on load", func() error {
		c := heavyClass(w.heavyRuns(t), "flutter-test")
		if len(c.Holders) != 0 || len(c.Waiters) != agents {
			return errorf("holders %d waiters %d", len(c.Holders), len(c.Waiters))
		}
		return nil
	})
	if n := running(); n != 0 {
		t.Fatalf("%d burners ran while the box read saturated", n)
	}

	var peak atomic.Int32
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			if n := int32(running()); n > peak.Load() {
				peak.Store(n)
			}
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	must(t, os.WriteFile(loadavg, []byte("0.10 1.00 1.00 2/300 1\n"), 0o644))

	left := agents
	for left > 0 {
		var now []string
		Eventually(t, "the next burners at the ceiling", func() error {
			now = now[:0]
			for _, id := range tasks {
				if _, err := os.Stat(filepath.Join(dir, "running", id)); err == nil {
					now = append(now, id)
				}
			}
			if len(now) != min(ceiling, left) {
				return errorf("running %v, want %d", now, min(ceiling, left))
			}
			return nil
		})
		for _, id := range now {
			release(t, dir, id)
		}
		for _, id := range now {
			w.WaitAgentLog(id, "step: 1 run ok")
		}
		left -= len(now)
	}
	close(stop)
	<-sampled
	if p := peak.Load(); p != ceiling {
		t.Fatalf("peak burners %d, want the ceiling %d", p, ceiling)
	}
	if got := strings.Count(readFile(filepath.Join(dir, "log")), "end "); got != agents {
		t.Fatalf("%d runs finished, want %d:\n%s", got, agents, logLines(dir))
	}
}
