package app

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/forge"
	"github.com/brandonapol/saddle/internal/gitx"
)

// stallingGit puts a git on PATH that hangs on push, as one does on a TCP
// connection that died silently, and runs the real git for anything else.
// The hung push forks a sleep that keeps git's pipes open.
func stallingGit(t *testing.T) {
	t.Helper()
	real, err := exec.LookPath("git")
	must(t, err)
	bin := t.TempDir()
	script := `#!/bin/sh
for a in "$@"; do
	if [ "$a" = push ]; then sleep 3600; exit 1; fi
done
exec "` + real + `" "$@"
`
	must(t, os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// #788: a push on a dead connection hung for 40 minutes while it held
// train.lock, and the whole train stopped. Network git calls now have a
// deadline: publishing returns ErrNetTimeout, a wait rather than a failure,
// and lets go of the lock.
func TestPRsStalledPushTimesOutAndReleasesTrainLock(t *testing.T) {
	a := trainSetup(t)
	originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	evs, err := a.Store.Events(-1)
	must(t, err)
	var cursor int64
	for _, e := range evs {
		cursor = max(cursor, e.ID)
	}

	stallingGit(t)
	old := gitx.NetTimeout
	gitx.NetTimeout = 2 * time.Second
	t.Cleanup(func() { gitx.NetTimeout = old })

	start := time.Now()
	_, err = a.PRs()
	took := time.Since(start)
	if !errors.Is(err, gitx.ErrNetTimeout) || !errors.Is(err, forge.ErrNetTimeout) {
		t.Fatalf("PRs over a stalled push = %v, want ErrNetTimeout", err)
	}
	if !forge.Transient(err) {
		t.Fatalf("Transient(%v) = false, want a wait/retry", err)
	}
	if took > 8*time.Second {
		t.Fatalf("PRs took %s over a 2s push deadline", took)
	}
	unlock, ok, err := a.TryLockTrain()
	must(t, err)
	if !ok {
		t.Fatal("train.lock still held after the push timed out")
	}
	unlock()

	after, err := a.Store.EventsSince(cursor)
	must(t, err)
	for _, e := range after {
		if strings.Contains(e.Kind, "fail") || strings.Contains(e.Kind, "error") || strings.Contains(e.Kind, "escalat") || e.Kind == "stack_flagged" {
			t.Errorf("a timed-out push logged %s: %s", e.Kind, e.Data)
		}
	}
}
