//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
)

// queueRun starts `saddle run --class go-test --prio prio` as task in the
// world's environment, so it shares the world's heavy-run queue.
func (w *World) queueRun(t *testing.T, task, prio, script string) *contender {
	t.Helper()
	cmd := exec.Command(w.Bins.Saddle, "run", "--class", "go-test", "--prio", prio, "--wait-max", "5m", "--", "sh", "-c", script)
	cmd.Env = append(w.Env(), "SADDLE_TASK="+task)
	cmd.Dir = t.TempDir()
	c := &contender{cmd: cmd, stderr: &syncBuf{}, done: make(chan error, 1)}
	cmd.Stderr = c.stderr
	must(t, cmd.Start())
	go func() { c.done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		err := <-c.done
		c.done <- err
	})
	return c
}

// TestJourneySpawnBackpressure (#243): workers' heavy runs back up the
// go-test queue (one slot). Gate runs waiting in it don't count. Once more
// than 2 × slots worker runs wait, spawn (MCP and CLI) refuses, naming the
// class, the queue length and the ETA, and the orchestrator hears about it;
// `saddle spawn --force` still works. When the queue drains, spawn works
// again.
func TestJourneySpawnBackpressure(t *testing.T) {
	w := world(t, Options{})
	must(t, os.MkdirAll(filepath.Join(w.Home, ".config", "saddle"), 0o755))
	must(t, os.WriteFile(filepath.Join(w.Home, ".config", "saddle", "runq.toml"), []byte(
		"mode = \"enforce\"\nheartbeat = \"1s\"\nmax_load_per_cpu = 0\nmax_cpu_pressure = 0\n[classes.go-test]\nslots = 1\n"), 0o644))

	gate := filepath.Join(t.TempDir(), "go")
	holder := w.queueRun(t, "tA", "worker", "while [ ! -f "+shq(gate)+" ]; do sleep 0.1; done")
	Eventually(t, "the holder to start", func() error {
		if r := w.Saddle("runq", "status"); !strings.Contains(r.Stdout, "tA") {
			return errorf("runq status:\n%s", r)
		}
		return nil
	})
	var waiters []*contender
	for _, task := range []string{"tT1", "tT2", "tT3"} {
		waiters = append(waiters, w.queueRun(t, task, "train", "true"))
	}
	for _, task := range []string{"tB", "tC"} {
		waiters = append(waiters, w.queueRun(t, task, "worker", "true"))
	}
	queued := func(n int) func() error {
		return func() error {
			r := w.Saddle("runq", "status")
			if !strings.Contains(r.Stdout, fmt.Sprintf("go-test: 1/1 slots busy, %d waiting", n)) {
				return errorf("want %d waiting:\n%s", n, r)
			}
			return nil
		}
	}
	Eventually(t, "five waiters", queued(5))
	// Two worker waiters on one slot is at 2 × slots, not over; the three
	// train runs never count.
	w.Spawn("t1", "Alpha work", []string{"alpha/**"}, fa.Wait("never-comes"))

	waiters = append(waiters, w.queueRun(t, "tD", "worker", "true"))
	Eventually(t, "six waiters", queued(6))

	out, isErr := w.MCP("t0", "spawn", map[string]any{"title": "Beta work", "prompt": "x", "claims": []string{"beta/**"}})
	if !isErr || !strings.Contains(out, "heavy-run queue backed up") || !strings.Contains(out, "go-test has 3 runs waiting for 1 slot") ||
		!strings.Contains(out, "no run-time history for an ETA") {
		t.Fatalf("MCP spawn with the queue backed up: err=%v %s", isErr, out)
	}
	r := w.Saddle("spawn", "--id", "t2", "Beta work", "x")
	if r.Code == 0 || !strings.Contains(r.String(), "go-test has 3 runs waiting") || !strings.Contains(r.String(), "saddle spawn --force") {
		t.Fatalf("CLI spawn with the queue backed up: %s", r)
	}
	if n := w.MustSaddle("notices", "--all").Stdout; !strings.Contains(n, "heavy-run queue for go-test is backed up") {
		t.Fatalf("the orchestrator wasn't told:\n%s", n)
	}

	must(t, fa.Script{Steps: []fa.Step{fa.Wait("never-comes")}}.Save(w.Scripts, "t3"))
	w.MustSaddle("spawn", "--force", "--id", "t3", "-c", "gamma/**", "Gamma work", "x")

	must(t, os.WriteFile(gate, nil, 0o644))
	holder.wait(t)
	for _, c := range waiters {
		c.wait(t)
	}
	w.Spawn("t2", "Beta work", []string{"beta/**"}, fa.Wait("never-comes"))
}
