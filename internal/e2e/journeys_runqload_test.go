//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These journeys drive the load gate and the heavy children's priority
// (#241) through real `saddle run` processes. The gate reads a fake /proc
// (SADDLE_RUNQ_PROC), so the journey controls how busy the box looks.

// loadShells is runqShells with a gate config and a fake /proc root.
func loadShells(t *testing.T, toml string) (runqShells, string) {
	t.Helper()
	s := newRunqShells(t)
	must(t, os.WriteFile(filepath.Join(s.config, "saddle", "runq.toml"),
		[]byte("mode = \"enforce\"\nheartbeat = \"1s\"\n"+toml), 0o644))
	proc := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(proc, "proc", "pressure"), 0o755))
	return s, proc
}

// runLoaded is runqShells.run with the gate reading proc.
func (s runqShells) runLoaded(t *testing.T, proc, task, script string) *contender {
	t.Helper()
	cmd := exec.Command(bins.Saddle, "run", "--class", "go-test", "--wait-max", "2m", "--", "sh", "-c", script)
	cmd.Env = append(s.env(task), "SADDLE_RUNQ_PROC="+proc)
	cmd.Dir = t.TempDir()
	c := &contender{cmd: cmd, stderr: &syncBuf{}, done: make(chan error, 1)}
	cmd.Stderr = c.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { c.done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		err := <-c.done
		c.done <- err
	})
	return c
}

// TestJourneySaddleRunWaitsOnLoad: an agent asks for a heavy run with a free
// slot while the box is saturated by other work. saddle run says it is
// waiting on load, starts by itself once the load drops, and runs the
// command at nice 10.
func TestJourneySaddleRunWaitsOnLoad(t *testing.T) {
	s, proc := loadShells(t, "max_load_per_cpu = 1.0\nmax_cpu_pressure = 60\ngate_max_wait = \"5m\"\n")
	loadavg := filepath.Join(proc, "proc", "loadavg")
	must(t, os.WriteFile(loadavg, []byte("999.00 1.00 1.00 2/300 1\n"), 0o644))
	out := filepath.Join(t.TempDir(), "out")
	a := s.runLoaded(t, proc, "ta", "echo ran > "+out+"; nice >> "+out)
	Eventually(t, "a held by the load gate", func() error {
		if st := a.stderr.String(); !strings.Contains(st, "queued: position 1 of 1 for go-test") ||
			!strings.Contains(st, "waiting on load: load ") {
			return errorf("a stderr %q", st)
		}
		return nil
	})
	if _, err := os.Stat(out); err == nil {
		t.Fatal("the command ran while the gate held it")
	}
	if st := s.status(t); len(st.Classes) != 1 || len(st.Classes[0].Waiters) != 1 || len(st.Classes[0].Holders) != 0 {
		t.Fatalf("status while gated %+v", st)
	}
	// Other work finishes; CPU PSI is low too.
	must(t, os.WriteFile(filepath.Join(proc, "proc", "pressure", "cpu"), []byte("some avg10=5.00 avg60=0 avg300=0 total=1\n"), 0o644))
	must(t, os.WriteFile(loadavg, []byte("0.10 1.00 1.00 2/300 1\n"), 0o644))
	if code := a.exitCode(t); code != 0 {
		t.Fatalf("a exited %d\n%s", code, a.stderr)
	}
	got, _ := os.ReadFile(out)
	lines := strings.Fields(string(got))
	base, err := exec.Command("nice").Output()
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(base)))
	if len(lines) != 2 || lines[0] != "ran" || lines[1] != strconv.Itoa(min(n+10, 19)) {
		t.Fatalf("command output %q, want ran and niceness %d", got, min(n+10, 19))
	}
	if strings.Count(a.stderr.String(), "waiting on load") != 1 {
		t.Fatalf("one waiting-on-load line expected:\n%s", a.stderr)
	}
}

// TestJourneySaddleRunGateGivesUp: a box that stays busy holds the run at
// most gate_max_wait; a box whose probes can't be read never holds it.
func TestJourneySaddleRunGateGivesUp(t *testing.T) {
	s, proc := loadShells(t, "gate_max_wait = \"2s\"\n")
	must(t, os.WriteFile(filepath.Join(proc, "proc", "loadavg"), []byte("999.00 1.00 1.00 2/300 1\n"), 0o644))
	start := time.Now()
	a := s.runLoaded(t, proc, "ta", "true")
	if code := a.exitCode(t); code != 0 {
		t.Fatalf("a exited %d\n%s", code, a.stderr)
	}
	if took := time.Since(start); took < 2*time.Second {
		t.Fatalf("started after %s, before gate_max_wait", took)
	}
	if st := a.stderr.String(); !strings.Contains(st, "waiting on load") || !strings.Contains(st, "slot acquired after") {
		t.Fatalf("a stderr %q", st)
	}

	// No /proc/loadavg (macOS, a container without it): fail open.
	b := s.runLoaded(t, t.TempDir(), "tb", "true")
	if code := b.exitCode(t); code != 0 {
		t.Fatalf("b exited %d\n%s", code, b.stderr)
	}
	if st := b.stderr.String(); strings.Contains(st, "queued") {
		t.Fatalf("an unreadable probe held the run:\n%s", st)
	}
}
