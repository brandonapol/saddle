package uplock

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func lockPath(t *testing.T) string { return filepath.Join(t.TempDir(), "up.lock") }

func TestSecondAcquireRefuses(t *testing.T) {
	p := lockPath(t)
	rel, err := Acquire(p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	_, err = Acquire(p, Options{})
	var held *HeldError
	if !errors.As(err, &held) {
		t.Fatalf("want HeldError, got %v", err)
	}
	if held.PID != os.Getpid() {
		t.Fatalf("pid = %d", held.PID)
	}
	if !strings.Contains(err.Error(), "--takeover") {
		t.Fatalf("message should name the escape hatch: %v", err)
	}
}

func TestStaleLockTakenOver(t *testing.T) {
	p := lockPath(t)
	// A pid file left by a process that is gone, with no flock held.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rel, err := Acquire(p, Options{})
	if err != nil {
		t.Fatalf("stale lock should be taken over: %v", err)
	}
	rel()
}

func TestReleasedOnRelease(t *testing.T) {
	p := lockPath(t)
	rel, err := Acquire(p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	rel()
	rel2, err := Acquire(p, Options{})
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	rel2()
}

// Helper process: takes the lock, prints READY, then sleeps until killed.
func TestMain(m *testing.M) {
	if p := os.Getenv("UPLOCK_HELPER"); p != "" {
		if _, err := Acquire(p, Options{}); err != nil {
			os.Exit(2)
		}
		_, _ = os.Stdout.WriteString("READY\n")
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func startHolder(t *testing.T, p string) (*exec.Cmd, <-chan struct{}) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "UPLOCK_HELPER="+p)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }() // reap, so a stopped holder is not a zombie
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
	buf := make([]byte, 6)
	if _, err := out.Read(buf); err != nil || string(buf) != "READY\n" {
		t.Fatalf("helper not ready: %q %v", buf, err)
	}
	return cmd, done
}

func TestLockReleasedOnProcessExit(t *testing.T) {
	p := lockPath(t)
	h, done := startHolder(t, p)
	if _, err := Acquire(p, Options{}); err == nil {
		t.Fatal("live holder should refuse")
	}
	_ = h.Process.Kill()
	<-done
	rel, err := Acquire(p, Options{})
	if err != nil {
		t.Fatalf("lock should be free after holder exit: %v", err)
	}
	rel()
}

func TestTakeoverStopsOldSession(t *testing.T) {
	p := lockPath(t)
	h, done := startHolder(t, p)
	rel, err := Acquire(p, Options{Takeover: true, Grace: 5 * time.Second})
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	defer rel()
	<-done // the old session must have been stopped
	if h.ProcessState == nil || h.ProcessState.Success() {
		t.Fatalf("old session not signalled: %v", h.ProcessState)
	}
}
