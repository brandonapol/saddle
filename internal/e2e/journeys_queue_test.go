//go:build e2e

package e2e

import (
	"strings"
	"testing"

	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
	"github.com/brandonapol/saddle/internal/mcpserver"
)

// queueLines is `saddle queue`, one entry per line ("1. t3 queued").
func (w *World) queueLines() []string {
	w.T.Helper()
	return strings.Split(strings.TrimSpace(w.MustSaddle("queue").Stdout), "\n")
}

// TestJourneyQueueMoveHoldRelease (#25): queue move sets the landing order,
// a held entry keeps its place while land skips it, calling done again
// doesn't release it, and once released it lands with its new work.
func TestJourneyQueueMoveHoldRelease(t *testing.T) {
	w := world(t, Options{})
	w.Spawn("t1", "alpha work", []string{"alpha/**"}, append(finished("alpha", "alpha\n"),
		fa.Wait("redo-now"), fa.Write("alpha/more.txt", "more\n"), fa.Commit("more alpha"), fa.Done("Adds more alpha."))...)
	w.Spawn("t2", "beta work", []string{"beta/**"}, finished("beta", "beta\n")...)
	w.Spawn("t3", "gamma work", []string{"gamma/**"}, finished("gamma", "gamma\n")...)
	for _, id := range []string{"t1", "t2", "t3"} {
		w.WaitTask(id, "queued in the train", func(v mcpserver.TaskView) bool { return v.Status == "done" && v.Train == "queued" })
	}

	// They called done in whatever order they finished; move them into a known one.
	w.MustSaddle("queue", "move", "t3", "1")
	w.MustSaddle("queue", "move", "t1", "2")
	if q := w.queueLines(); strings.Join(q, "|") != "1. t3 queued|2. t1 queued|3. t2 queued" {
		t.Fatalf("queue after moves = %q", q)
	}
	// Past the end means the back.
	w.MustSaddle("queue", "move", "t3", "99")
	if q := w.queueLines(); strings.Join(q, "|") != "1. t1 queued|2. t2 queued|3. t3 queued" {
		t.Fatalf("queue after moving t3 past the end = %q", q)
	}
	w.MustSaddle("queue", "move", "t3", "1")

	if r := w.MustSaddle("queue", "hold", "t1", "waiting", "on", "review"); !strings.Contains(r.Stdout, "t1 is on hold") {
		t.Fatalf("hold: %s", r)
	}
	if q := w.queueLines(); strings.Join(q, "|") != "1. t3 queued|2. t1 on_hold: waiting on review|3. t2 queued" {
		t.Fatalf("queue after hold = %q", q)
	}
	if r := w.Saddle("queue", "release", "t2"); r.Code == 0 || !strings.Contains(r.Stderr, "isn't on hold") {
		t.Fatalf("releasing an entry that isn't held: %s", r)
	}

	// t1 does more work and calls done again: it stays held, in its place.
	w.MustSaddle("message", "t1", "redo-now")
	w.WaitAgentLog("t1", "step: 7 done ok")
	if v := w.Task("t1"); !strings.HasPrefix(v.Train, "on_hold") {
		t.Fatalf("done released the hold: %+v", v)
	}
	if q := w.queueLines(); strings.Join(q, "|") != "1. t3 queued|2. t1 on_hold: waiting on review|3. t2 queued" {
		t.Fatalf("queue after the held task called done again = %q", q)
	}

	// land skips the held entry and lands the others in queue order.
	w.MustSaddle("land")
	for _, id := range []string{"t2", "t3"} {
		if v := w.Task(id); v.Status != "landed" {
			t.Fatalf("%s = %+v, want landed", id, v)
		}
	}
	if v := w.Task("t1"); v.Status == "landed" || !strings.HasPrefix(v.Train, "on_hold") {
		t.Fatalf("held t1 = %+v, want still on hold", v)
	}
	if got := w.Git(w.Repo, "log", "--reverse", "--format=%s", "main..saddle/integration"); got != "work in gamma\nwork in beta" {
		t.Fatalf("integration landed %q, want gamma then beta", got)
	}
	if q := w.queueLines(); strings.Join(q, "|") != "1. t1 on_hold: waiting on review" {
		t.Fatalf("queue after land = %q", q)
	}

	if r := w.MustSaddle("queue", "release", "t1"); !strings.Contains(r.Stdout, "t1 is back in line") {
		t.Fatalf("release: %s", r)
	}
	w.MustSaddle("land")
	if v := w.Task("t1"); v.Status != "landed" {
		t.Fatalf("t1 after release and land = %+v", v)
	}
	files := w.Git(w.Repo, "ls-tree", "-r", "--name-only", "saddle/integration")
	if !strings.Contains(files, "alpha/work.txt") || !strings.Contains(files, "alpha/more.txt") {
		t.Fatalf("integration lacks t1's work, first and second done:\n%s", files)
	}
	if r := w.MustSaddle("queue"); !strings.Contains(r.Stdout, "the queue is empty") {
		t.Fatalf("queue after everything landed: %s", r)
	}
}
