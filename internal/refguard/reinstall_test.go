package refguard

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/brandonapol/saddle/internal/gitx"
)

// Every spawn and land runs saddle init, which reinstalls the hooks while
// agents commit in their worktrees. Install used to remove each hook and
// write it again, so a commit landing in that window failed: git found no
// reference-transaction hook to run, or ran a half-written one (seen as TestJourneyAutopilotDrainsReadyQueue flaking under load).

// TestReinstallKeepsUnchangedHooks: reinstalling the same hooks leaves the
// files alone, so nothing git might be running is ever replaced.
func TestReinstallKeepsUnchangedHooks(t *testing.T) {
	r := setup(t)
	hooks, err := gitx.Run(r.root, "rev-parse", "--path-format=absolute", "--git-path", "hooks")
	if err != nil {
		t.Fatal(err)
	}
	inodes := func() map[string]uint64 {
		out := map[string]uint64{}
		for _, name := range []string{"reference-transaction", "pre-push"} {
			fi, err := os.Stat(filepath.Join(hooks, name))
			if err != nil {
				t.Fatal(err)
			}
			out[name] = fi.Sys().(*syscall.Stat_t).Ino
		}
		return out
	}
	before := inodes()
	if err := Install(r.root, r.bin); err != nil {
		t.Fatal(err)
	}
	for name, ino := range inodes() {
		if ino != before[name] {
			t.Errorf("%s was rewritten by a reinstall with the same content", name)
		}
	}
}

// TestReinstallWhileCommitting: commits made while Install runs over and
// over, with a changing binary path so each run really rewrites the hooks,
// all succeed.
func TestReinstallWhileCommitting(t *testing.T) {
	r := setup(t)
	// A second path to the same binary, so alternate installs differ.
	alt := filepath.Join(t.TempDir(), "saddle")
	if err := os.Symlink(r.bin, alt); err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; !stop.Load(); i++ {
			bin := r.bin
			if i%2 == 1 {
				bin = alt
			}
			if err := Install(r.root, bin); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	defer func() { stop.Store(true); wg.Wait() }()
	for i := range 40 {
		if err := r.run("", "commit", "-q", "--allow-empty", "-m", "c"); err != nil {
			t.Fatalf("commit %d while hooks were reinstalled: %v", i, err)
		}
	}
}
