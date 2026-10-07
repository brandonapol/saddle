//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/store"
)

// TestJourneyGateEnvFailureDoesNotBounce (#184): the test gate hits "disk
// quota exceeded" once, then passes. The gate is the environment's
// problem, not t1's: `saddle land` retries it with its temp dirs outside
// the repo (under the user cache dir), t1 is never returned to its producer or charged an attempt,
// and it lands.
func TestJourneyGateEnvFailureDoesNotBounce(t *testing.T) {
	scratch := t.TempDir()
	once, seen := filepath.Join(scratch, "failed-once"), filepath.Join(scratch, "tmpdirs")
	gate := "if [ ! -e " + once + " ]; then touch " + once + "; echo \"open $TMPDIR/TestX/db: disk quota exceeded\"; exit 1; fi; " +
		"echo \"$TMPDIR\" >> " + seen
	w := world(t, Options{TestCmd: gate, Tables: "[train]\nno_auto_rebase = true\n"})
	w.Spawn("t1", "Alpha work", []string{"alpha/**"}, finished("alpha", "alpha\n")...)
	w.WaitTask("t1", "queued", func(v mcpserver.TaskView) bool { return v.Train == "queued" })

	r := w.Saddle("land")
	if v := w.Task("t1"); v.Status != "landed" {
		t.Fatalf("t1 bounced on an environment failure: %+v\n%s", v, r)
	}
	a := w.App()
	b, err := os.ReadFile(seen)
	must(t, err)
	// A temp dir inside the repo sits under its .saddle/config.toml, where
	// tests that look for a saddle repo find the real one.
	for _, dir := range strings.Fields(string(b)) {
		if rel, err := filepath.Rel(w.Repo, dir); err != nil || !strings.HasPrefix(rel, "..") {
			t.Fatalf("gate TMPDIR = %q, inside the repo %s", dir, w.Repo)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("gate TMPDIR %s left behind (%v)", dir, err)
		}
	}
	es, err := a.Store.Train()
	must(t, err)
	for _, e := range es {
		if e.Task == "t1" && e.Attempts != 0 {
			t.Fatalf("the environment failure counted against t1: %+v", e)
		}
	}
	evs, err := a.Store.Events(500)
	must(t, err)
	if !slices.ContainsFunc(evs, func(e store.Event) bool { return e.Task == "t1" && e.Kind == app.EventGateEnv }) {
		t.Fatalf("no %s event for t1's retried gate", app.EventGateEnv)
	}
	if slices.ContainsFunc(evs, func(e store.Event) bool { return e.Task == "t1" && e.Kind == "train_"+store.TestFailed }) {
		t.Fatal("the environment failure was recorded as t1's test failure")
	}
}
