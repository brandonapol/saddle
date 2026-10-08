//go:build e2e

package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/mcpserver"
)

// gateRunqWorld is a World whose heavy-run queue enforces, with one go-test
// slot that the train's test.cmd falls in (#239).
func gateRunqWorld(t *testing.T, testCmd string) *World {
	t.Helper()
	w := world(t, Options{TestCmd: testCmd})
	must(t, os.MkdirAll(filepath.Join(w.Home, ".config", "saddle"), 0o755))
	must(t, os.WriteFile(filepath.Join(w.Home, ".config", "saddle", "runq.toml"),
		[]byte("mode = \"enforce\"\nmax_load_per_cpu = 0\nmax_cpu_pressure = 0\nheartbeat = \"1s\"\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(w.Repo, ".saddle", "runq.toml"),
		[]byte("[classes.go-test]\nslots = 1\nmatch = [\"sh *gate.sh\", \"go test*\"]\n"), 0o644))
	return w
}

// TestJourneyGateHookRidesLease (#239): the train's test gate holds the only
// go-test slot. Its test.cmd runs git commit, whose pre-commit hook runs
// `saddle run --class go-test`. The hook rides on the gate's lease through
// SADDLE_RUNQ_LEASE instead of queueing behind it, so the land finishes.
func TestJourneyGateHookRidesLease(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "gate.sh")
	w := gateRunqWorld(t, "sh "+gate)
	log := filepath.Join(w.Root, "hook.log")
	hook := "#!/bin/sh\nexec saddle run --class go-test --wait-max 20s -- sh -c 'echo \"hook ran in $SADDLE_RUNQ_LEASE\"' >> " + shq(log) + " 2>&1\n"
	must(t, os.WriteFile(gate, []byte("set -e\n"+
		"d=$(mktemp -d)\n"+
		"git init -q \"$d\"\n"+
		"printf '%s' "+shq(hook)+" > \"$d/.git/hooks/pre-commit\"\n"+
		"chmod +x \"$d/.git/hooks/pre-commit\"\n"+
		"git -C \"$d\" commit -q --allow-empty -m gate\n"+
		"echo \"gate held $SADDLE_RUNQ_LEASE\" >> "+shq(log)+"\n"), 0o644))

	w.Spawn("t1", "Alpha work", []string{"alpha/**"}, finished("alpha", "alpha\n")...)
	w.WaitTask("t1", "queued in the train", func(v mcpserver.TaskView) bool { return v.Status == "done" && v.Train == "queued" })
	_ = os.Remove(log) // done may have run the gate's command already

	start := time.Now()
	r := w.MustSaddle("land")
	if v := w.Task("t1"); v.Status != "landed" {
		t.Fatalf("t1 didn't land: %+v\n%s\nhook log:\n%s", v, r, readFile(log))
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("land took %s: the hook queued behind its own gate", d)
	}
	out := readFile(log)
	if strings.Contains(out, "queued:") {
		t.Fatalf("the hook queued behind the gate:\n%s", out)
	}
	var hookTok, gateTok string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if v, ok := strings.CutPrefix(l, "hook ran in "); ok {
			hookTok = v
		}
		if v, ok := strings.CutPrefix(l, "gate held "); ok {
			gateTok = v
		}
	}
	if gateTok == "" || hookTok != gateTok {
		t.Fatalf("the hook (%q) didn't ride the gate's lease (%q):\n%s", hookTok, gateTok, out)
	}
}

// TestJourneyGateWaitShowsQueuedInTrain (#239): a worker's `saddle run`
// holds the only go-test slot. `saddle land` waits for it at gate priority,
// and meanwhile the train shows "queued: position 1 of 1 for go-test,
// holder t83 …". Once the worker's run ends the gate runs and t1 lands.
func TestJourneyGateWaitShowsQueuedInTrain(t *testing.T) {
	w := gateRunqWorld(t, "go test -h >/dev/null 2>&1; true")
	w.Spawn("t1", "Alpha work", []string{"alpha/**"}, finished("alpha", "alpha\n")...)
	w.WaitTask("t1", "queued in the train", func(v mcpserver.TaskView) bool { return v.Status == "done" && v.Train == "queued" })

	release := filepath.Join(w.Root, "release")
	holder := exec.Command(w.Bins.Saddle, "run", "--class", "go-test", "--", "sh", "-c",
		"while [ ! -e "+shq(release)+" ]; do sleep 0.1; done")
	holder.Env = append(w.Env(), "SADDLE_TASK=t83")
	holder.Dir = w.Repo // reads the repo's runq.toml, so go-test has one slot
	must(t, holder.Start())
	t.Cleanup(func() { _ = holder.Process.Kill(); _ = holder.Wait() })
	waitFor(t, "the worker holds go-test", func() bool {
		return strings.Contains(w.MustSaddle("runq", "status").Stdout, "t83")
	})

	land := exec.Command(w.Bins.Saddle, "land")
	land.Env, land.Dir = w.Env(), w.Repo
	var landOut bytes.Buffer
	land.Stdout, land.Stderr = &landOut, &landOut
	must(t, land.Start())
	landed := make(chan error, 1)
	go func() { landed <- land.Wait() }()
	t.Cleanup(func() { _ = land.Process.Kill() })

	v := w.WaitTask("t1", "the train shows the gate's wait", func(v mcpserver.TaskView) bool {
		return strings.HasPrefix(v.Train, "queued: position 1 of 1 for go-test, holder t83")
	})
	if q := w.MustSaddle("queue"); !strings.Contains(q.Stdout, "t1 queued: position 1 of 1 for go-test") {
		t.Fatalf("saddle queue doesn't show the wait:\n%s", q.Stdout)
	}
	select {
	case err := <-landed:
		t.Fatalf("land finished while the worker held the slot (%v): %+v\n%s", err, v, landOut.String())
	default:
	}

	must(t, os.WriteFile(release, nil, 0o644))
	select {
	case err := <-landed:
		if err != nil {
			t.Fatalf("land: %v\n%s", err, landOut.String())
		}
	case <-time.After(Timeout()):
		t.Fatalf("land didn't finish after the slot freed:\n%s", landOut.String())
	}
	if v := w.Task("t1"); v.Status != "landed" {
		t.Fatalf("t1 didn't land: %+v\n%s", v, landOut.String())
	}
}

func readFile(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(Timeout()); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting: %s", what)
}
