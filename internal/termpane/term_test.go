package termpane

import (
	"strings"
	"testing"
	"time"
)

// waitFor polls the rendered screen until it contains want.
func waitFor(t *testing.T, tm *Term, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(tm.Text(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("screen never showed %q; got:\n%s", want, tm.Text())
}

func send(t *testing.T, tm *Term, s string) {
	t.Helper()
	if err := tm.Send([]byte(s)); err != nil {
		t.Fatal(err)
	}
}

func startSh(t *testing.T, cols, rows int) *Term {
	t.Helper()
	tm, err := Start("/bin/sh", t.TempDir(), cols, rows)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tm.Close() })
	return tm
}

func TestStartRunsShellInDirWithPty(t *testing.T) {
	dir := t.TempDir()
	tm, err := Start("/bin/sh", dir, 80, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer tm.Close()
	send(t, tm, "pwd; test -t 0 && echo IS-A-TTY\r")
	waitFor(t, tm, dir)
	waitFor(t, tm, "IS-A-TTY")
}

func TestOutputSignalsUpdates(t *testing.T) {
	tm := startSh(t, 80, 10)
	send(t, tm, "echo hi\r")
	select {
	case <-tm.Updates():
	case <-time.After(5 * time.Second):
		t.Fatal("no update after output")
	}
}

func TestResizeReachesShell(t *testing.T) {
	tm := startSh(t, 80, 10)
	if err := tm.Resize(57, 13); err != nil {
		t.Fatal(err)
	}
	if c, r := tm.Size(); c != 57 || r != 13 {
		t.Fatalf("screen size = %dx%d, want 57x13", c, r)
	}
	send(t, tm, "stty size\r")
	waitFor(t, tm, "13 57")
}

func TestShellExitClosesDone(t *testing.T) {
	tm := startSh(t, 80, 10)
	if tm.Exited() {
		t.Fatal("exited too early")
	}
	send(t, tm, "exit 3\r")
	select {
	case <-tm.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done not closed after the shell exited")
	}
	if !tm.Exited() {
		t.Fatal("Exited() = false after Done")
	}
	if err := tm.Send([]byte("x")); err == nil {
		t.Fatal("write after exit should fail")
	}
}

func TestCloseKillsShell(t *testing.T) {
	tm := startSh(t, 80, 10)
	send(t, tm, "sleep 100\r")
	tm.Close()
	select {
	case <-tm.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not end the shell")
	}
}

func TestInteractiveInputReachesProgram(t *testing.T) {
	tm := startSh(t, 80, 10)
	send(t, tm, "read x; echo got:$x\r")
	send(t, tm, "abc\r")
	waitFor(t, tm, "got:abc")
}

func TestStartBadShellFails(t *testing.T) {
	if _, err := Start("/nonexistent/shell", t.TempDir(), 80, 10); err == nil {
		t.Fatal("expected an error")
	}
}
