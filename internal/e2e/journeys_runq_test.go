//go:build e2e

package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The heavy-run scheduler spike (#236) isn't wired into saddle yet, so these
// journeys drive its stand-in CLI (internal/runq/cmd/runq) as real, separate
// contender processes sharing one queue file.

var (
	runqOnce sync.Once
	runqBin  string
	runqErr  error
)

func buildRunq(t *testing.T) string {
	t.Helper()
	runqOnce.Do(func() {
		root, err := moduleRoot()
		if err != nil {
			runqErr = err
			return
		}
		runqBin = filepath.Join(bins.Dir, "runq")
		cmd := exec.Command("go", "build", "-o", runqBin, "./internal/runq/cmd/runq")
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			runqErr = errorf("go build runq: %v\n%s", err, out)
		}
	})
	if runqErr != nil {
		t.Fatal(runqErr)
	}
	return runqBin
}

// syncBuf is a goroutine-safe buffer for a contender's stderr.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type contender struct {
	cmd    *exec.Cmd
	stderr *syncBuf
	done   chan error
}

// contend starts `runq run -class class -- sh -c script` as task.
func contend(t *testing.T, bin, db, task, class, script string) *contender {
	t.Helper()
	cmd := exec.Command(bin, "-db", db, "-heartbeat", "1s", "run", "-class", class, "--", "sh", "-c", script)
	cmd.Env = append(os.Environ(), "SADDLE_TASK="+task, "SADDLE_RUNQ=", "SADDLE_RUNQ_LEASE=")
	c := &contender{cmd: cmd, stderr: &syncBuf{}, done: make(chan error, 1)}
	cmd.Stderr = c.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { c.done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-c.done
		c.done <- nil
	})
	return c
}

func (c *contender) wait(t *testing.T) {
	t.Helper()
	select {
	case err := <-c.done:
		c.done <- err
		if err != nil {
			t.Fatalf("%v: %v\n%s", c.cmd.Args, err, c.stderr)
		}
	case <-time.After(Timeout()):
		t.Fatalf("%v still running\n%s", c.cmd.Args, c.stderr)
	}
}

func runqStatus(t *testing.T, bin, db string) string {
	t.Helper()
	out, err := exec.Command(bin, "-db", db, "status").CombinedOutput()
	if err != nil {
		t.Fatalf("runq status: %v\n%s", err, out)
	}
	return string(out)
}

func fileHas(path, want string) func() error {
	return func() error {
		b, _ := os.ReadFile(path)
		if !strings.Contains(string(b), want) {
			return errorf("%s has %q, want %q", path, b, want)
		}
		return nil
	}
}

// TestJourneyRunqTwoContendersTakeTurns: two agents in two processes ask to
// run flutter tests with one slot. The second sees its position and the
// holder, starts by itself when the first ends, and the runs never overlap.
func TestJourneyRunqTwoContendersTakeTurns(t *testing.T) {
	bin := buildRunq(t)
	dir := t.TempDir()
	db, log := filepath.Join(dir, "runq.db"), filepath.Join(dir, "log")
	gate := filepath.Join(dir, "release-a")
	a := contend(t, bin, db, "ta", "flutter-test",
		`echo start-a >> `+log+`; while [ ! -e `+gate+` ]; do sleep 0.05; done; echo end-a >> `+log)
	Eventually(t, "a running", fileHas(log, "start-a"))
	b := contend(t, bin, db, "tb", "flutter-test", `echo start-b >> `+log+`; echo end-b >> `+log)
	Eventually(t, "b queued behind a", func() error {
		if s := b.stderr.String(); !strings.Contains(s, "queued: position 1 of 1 for flutter-test, holder ta (sh -c") {
			return errorf("b stderr %q", s)
		}
		return nil
	})
	if st := runqStatus(t, bin, db); !strings.Contains(st, "flutter-test: 1/1 slots busy, 1 waiting") ||
		!strings.Contains(st, "running ta") || !strings.Contains(st, "#1 tb") {
		t.Fatalf("status:\n%s", st)
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	a.wait(t)
	b.wait(t)
	got, _ := os.ReadFile(log)
	if strings.Join(strings.Fields(string(got)), " ") != "start-a end-a start-b end-b" {
		t.Fatalf("runs overlapped or misordered:\n%s", got)
	}
	if !strings.Contains(b.stderr.String(), "runq: flutter-test slot acquired after") {
		t.Fatalf("b stderr %q", b.stderr)
	}
}

// TestJourneyRunqCrashedHolder: SIGKILL the holder's runq process. Its
// child dies with it (no orphaned heavy run) and the waiter starts within
// one heartbeat (1s here).
func TestJourneyRunqCrashedHolder(t *testing.T) {
	bin := buildRunq(t)
	dir := t.TempDir()
	db, pidFile, log := filepath.Join(dir, "runq.db"), filepath.Join(dir, "pid"), filepath.Join(dir, "log")
	a := contend(t, bin, db, "ta", "go-test", `echo $$ > `+pidFile+`.tmp; mv `+pidFile+`.tmp `+pidFile+`; exec sleep 600`)
	Eventually(t, "a running", func() error {
		if _, err := os.Stat(pidFile); err != nil {
			return err
		}
		return nil
	})
	b := contend(t, bin, db, "tb", "go-test", `echo started >> `+log)
	Eventually(t, "b queued", func() error {
		if !strings.Contains(b.stderr.String(), "queued: position 1 of 1 for go-test, holder ta") {
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
	if took := time.Since(killed); took > time.Second+500*time.Millisecond {
		t.Fatalf("b started %s after the holder died; want about one heartbeat (1s)", took)
	}
	b.wait(t)
	Eventually(t, "the dead holder's child to die too", func() error {
		if err := syscall.Kill(child, 0); err == nil {
			return errorf("pid %d still alive", child)
		}
		return nil
	})
	if st := runqStatus(t, bin, db); !strings.Contains(st, "go-test: 0/1 slots busy, 0 waiting") {
		t.Fatalf("status after crash:\n%s", st)
	}
}

// TestJourneyRunqNestedRunDoesNotDeadlock: a run holding the only slot
// starts another `runq run` of the same class (a pre-commit hook's make
// check inside an agent's run); the inner run rides on the outer lease.
func TestJourneyRunqNestedRunDoesNotDeadlock(t *testing.T) {
	bin := buildRunq(t)
	dir := t.TempDir()
	db, log := filepath.Join(dir, "runq.db"), filepath.Join(dir, "log")
	outer := contend(t, bin, db, "ta", "go-test",
		bin+` -db `+db+` run -class go-test -- sh -c 'echo inner-ran >> `+log+`'`)
	outer.wait(t)
	if err := fileHas(log, "inner-ran")(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(outer.stderr.String(), "queued") {
		t.Fatalf("the inner run queued behind its own parent:\n%s", outer.stderr)
	}
}
