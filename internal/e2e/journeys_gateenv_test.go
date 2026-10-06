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
// problem, not t1's: `saddle land` retries it with its temp dirs under
// .saddle/tmp, t1 is never returned to its producer or charged an attempt,
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
	if dir := strings.TrimSpace(string(b)); !strings.HasPrefix(dir, filepath.Join(w.Repo, ".saddle", "tmp")+string(filepath.Separator)) {
		t.Fatalf("gate TMPDIR = %q, want a dir under .saddle/tmp", dir)
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
