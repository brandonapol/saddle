//go:build e2e

package e2e

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These journeys drive `saddle run` and `saddle runq` (#238) as real,
// separate processes. Each contender is a shell with no HOME and its own
// working directory outside any repo; they share only XDG_STATE_HOME, which
// is where the machine's queue lives.

// runqShells is the shared machine state for one journey: the state dir
// holding the queue and a user config with a 1s heartbeat in enforce mode.
type runqShells struct {
	state, config string
}

func newRunqShells(t *testing.T) runqShells {
	t.Helper()
	s := runqShells{state: t.TempDir(), config: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(s.config, "saddle"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.config, "saddle", "runq.toml"), []byte("mode = \"enforce\"\nmax_load_per_cpu = 0\nmax_cpu_pressure = 0\nheartbeat = \"1s\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return s
}

// env is a HOME-less shell environment for task.
func (s runqShells) env(task string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "HOME" || strings.HasPrefix(k, "SADDLE_") || strings.HasPrefix(k, "XDG_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "SADDLE_TASK="+task, "XDG_STATE_HOME="+s.state, "XDG_CONFIG_HOME="+s.config)
}

// run starts `saddle run --class class -- sh -c script` as task in its own
// directory.
func (s runqShells) run(t *testing.T, task, class, script string) *contender {
	t.Helper()
	cmd := exec.Command(bins.Saddle, "run", "--class", class, "--wait-max", "2m", "--", "sh", "-c", script)
	cmd.Env = s.env(task)
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

// exitCode waits for c and returns its exit code.
func (c *contender) exitCode(t *testing.T) int {
	t.Helper()
	select {
	case err := <-c.done:
		c.done <- err
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		if err != nil {
			t.Fatalf("%v: %v\n%s", c.cmd.Args, err, c.stderr)
		}
		return 0
	case <-time.After(Timeout()):
		t.Fatalf("%v still running\n%s", c.cmd.Args, c.stderr)
	}
	return -1
}

type runqJSON struct {
	Mode    string `json:"mode"`
	Classes []struct {
		Class   string `json:"class"`
		Slots   int    `json:"slots"`
		Holders []struct {
			Label string `json:"label"`
			Lease string `json:"lease"`
		} `json:"holders"`
		Waiters []struct {
			Label    string `json:"label"`
			Position int    `json:"position"`
		} `json:"waiters"`
	} `json:"classes"`
}

func (s runqShells) status(t *testing.T) runqJSON {
	t.Helper()
	cmd := exec.Command(bins.Saddle, "runq", "status", "--json")
	cmd.Env = s.env("")
	cmd.Dir = t.TempDir()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("saddle runq status --json: %v\n%s", err, out)
	}
	var st runqJSON
	if err := json.Unmarshal(out, &st); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return st
}

// TestJourneySaddleRunTwoShellsTakeTurns: two agents in two HOME-less
// shells ask to run flutter tests with one slot. The second sees its
// position and the holder, runq status --json shows both, and it starts by
// itself when the first ends. Exit codes come through.
func TestJourneySaddleRunTwoShellsTakeTurns(t *testing.T) {
	s := newRunqShells(t)
	dir := t.TempDir()
	log, gate := filepath.Join(dir, "log"), filepath.Join(dir, "release-a")
	a := s.run(t, "ta", "flutter-test",
		`echo start-a >> `+log+`; while [ ! -e `+gate+` ]; do sleep 0.05; done; echo end-a >> `+log)
	Eventually(t, "a running", fileHas(log, "start-a"))
	b := s.run(t, "tb", "flutter-test", `echo start-b >> `+log+`; echo end-b >> `+log+`; exit 4`)
	Eventually(t, "b queued behind a", func() error {
		if st := b.stderr.String(); !strings.Contains(st, "queued: position 1 of 1 for flutter-test, holder ta (sh -c") ||
			!strings.Contains(st, "(starts automatically; this command may take a while)") {
			return errorf("b stderr %q", st)
		}
		return nil
	})
	st := s.status(t)
	if st.Mode != "enforce" || len(st.Classes) != 1 {
		t.Fatalf("status %+v", st)
	}
	c := st.Classes[0]
	if c.Class != "flutter-test" || c.Slots != 1 || len(c.Holders) != 1 || c.Holders[0].Label != "ta" ||
		len(c.Waiters) != 1 || c.Waiters[0].Label != "tb" || c.Waiters[0].Position != 1 {
		t.Fatalf("status %+v", st)
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if code := a.exitCode(t); code != 0 {
		t.Fatalf("a exited %d\n%s", code, a.stderr)
	}
	if code := b.exitCode(t); code != 4 {
		t.Fatalf("b exited %d, want its command's 4\n%s", code, b.stderr)
	}
	got, _ := os.ReadFile(log)
	if strings.Join(strings.Fields(string(got)), " ") != "start-a end-a start-b end-b" {
		t.Fatalf("runs overlapped or misordered:\n%s", got)
	}
	if !strings.Contains(b.stderr.String(), "runq: flutter-test slot acquired after") {
		t.Fatalf("b stderr %q", b.stderr)
	}
}

// TestJourneySaddleRunKilledHolderFreesSlot: SIGKILL the holder's saddle
// run. Its command dies with it and the waiter starts within about one
// heartbeat (1s).
func TestJourneySaddleRunKilledHolderFreesSlot(t *testing.T) {
	s := newRunqShells(t)
	dir := t.TempDir()
	pidFile, log := filepath.Join(dir, "pid"), filepath.Join(dir, "log")
	a := s.run(t, "ta", "go-test", `echo $$ > `+pidFile+`.tmp; mv `+pidFile+`.tmp `+pidFile+`; exec sleep 600`)
	Eventually(t, "a running", func() error {
		_, err := os.Stat(pidFile)
		return err
	})
	// go-test has two slots by default; fill the second so b has to wait.
	s2 := s.run(t, "tc", "go-test", `exec sleep 600`)
	Eventually(t, "both go-test slots held", func() error {
		if st := s.status(t); len(st.Classes) != 1 || len(st.Classes[0].Holders) != 2 {
			return errorf("status %+v", st)
		}
		return nil
	})
	b := s.run(t, "tb", "go-test", `echo started >> `+log)
	Eventually(t, "b queued", func() error {
		if !strings.Contains(b.stderr.String(), "queued: position 1 of 1 for go-test") {
			return errorf("b stderr %q", b.stderr)
		}
		return nil
	})
	raw, _ := os.ReadFile(pidFile)
	child, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	killed := time.Now()
	if err := a.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	Eventually(t, "b started", fileHas(log, "started"))
	if took := time.Since(killed); took > 2*time.Second {
		t.Fatalf("b started %s after the holder died; want about one heartbeat (1s)", took)
	}
	if code := b.exitCode(t); code != 0 {
		t.Fatalf("b exited %d\n%s", code, b.stderr)
	}
	Eventually(t, "the dead holder's command to die too", func() error {
		if err := syscall.Kill(child, 0); err == nil {
			return errorf("pid %d still alive", child)
		}
		return nil
	})
	_ = s2.cmd.Process.Signal(syscall.SIGTERM)
	if code := s2.exitCode(t); code != 128+int(syscall.SIGTERM) {
		t.Fatalf("SIGTERM to saddle run: exit %d, want the forwarded signal's %d\n%s", code, 128+int(syscall.SIGTERM), s2.stderr)
	}
}

// TestJourneySaddleRunNestedDoesNotDeadlock: a saddle run holding the only
// slot runs another saddle run of the same class (a pre-commit hook's make
// check inside an agent's run). The inner run rides on the outer lease.
func TestJourneySaddleRunNestedDoesNotDeadlock(t *testing.T) {
	s := newRunqShells(t)
	log := filepath.Join(t.TempDir(), "log")
	outer := s.run(t, "ta", "e2e",
		bins.Saddle+` run --class e2e --wait-max 30s -- sh -c 'echo inner-ran >> `+log+`'`)
	if code := outer.exitCode(t); code != 0 {
		t.Fatalf("outer exited %d\n%s", code, outer.stderr)
	}
	if err := fileHas(log, "inner-ran")(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(outer.stderr.String(), "queued") {
		t.Fatalf("the inner run queued behind its own parent:\n%s", outer.stderr)
	}
}
