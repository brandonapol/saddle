//go:build e2e

package e2e

import (
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/brandonapol/saddle/internal/automerge"
	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
	"github.com/brandonapol/saddle/internal/e2e/fakegh"
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

// TestJourneyAutomergeHoldKeepsGreenStack: three green stacks on main with
// auto-merge on, the first held. Auto-merge passes the held one and merges
// the others, one per tick, says the held stack waits on its hold, and
// merges it once released.
func TestJourneyAutomergeHoldKeepsGreenStack(t *testing.T) {
	w := world(t, Options{})
	urls := landThree(t, w)
	must(t, w.GH.SetAllChecks(fakegh.Pass))
	w.MustSaddle("automerge", "on")
	if r := w.MustSaddle("automerge", "hold", "t1"); !strings.Contains(r.Stdout, "t1") {
		t.Fatalf("automerge hold: %s", r)
	}

	for _, want := range []string{"t2", "t3"} {
		st := w.AutomergeTick()
		if st.Merged != urls[want] {
			t.Fatalf("tick merged %q, want %s's %s past the held t1: %+v", st.Merged, want, urls[want], st)
		}
	}
	st := w.AutomergeTick()
	if st.Merged != "" || len(st.Stacks) != 1 || st.Stacks[0].ID != "t1" || !st.Stacks[0].Held {
		t.Fatalf("with only the held stack left: %+v", st)
	}
	if p := prNumber(t, w.GHState(), urls["t1"]); p.State != "OPEN" {
		t.Fatalf("held t1 = %s, want still open", p.State)
	}
	if out := w.MustSaddle("automerge", "status").Stdout; !strings.Contains(out, "stack t1") || !strings.Contains(out, "held") {
		t.Fatalf("status doesn't say t1 is held:\n%s", out)
	}

	w.MustSaddle("automerge", "release", "t1")
	if st := w.AutomergeTick(); st.Merged != urls["t1"] {
		t.Fatalf("after release the tick merged %q, want t1's %s: %+v", st.Merged, urls["t1"], st)
	}
	for _, f := range []string{"alpha/work.txt", "beta/work.txt", "gamma/work.txt"} {
		if w.OriginFile("main", f) == "" {
			t.Fatalf("main lacks %s", f)
		}
	}
}

// pausingGH is the real gh client, except that its first PR read signals
// started and waits for proceed: a check caught mid-way through reading
// GitHub.
type pausingGH struct {
	automerge.GitHub
	once             sync.Once
	started, proceed chan struct{}
}

func (g *pausingGH) PR(url string) (automerge.PR, error) {
	g.once.Do(func() {
		close(g.started)
		<-g.proceed
	})
	return g.GitHub.PR(url)
}

// TestJourneyAutomergeHoldDuringCheckSticks (#209): a hold (or on,
// off, release) made while the watcher is in the middle of a check must
// survive that check. Today Check saves the whole state it loaded before
// reading GitHub, so the hold is silently dropped and the held stack can
// merge on the next tick.
func TestJourneyAutomergeHoldDuringCheckSticks(t *testing.T) {
	t.Skip("#209: a check saves the state it loaded first, dropping holds and toggles made meanwhile")
	w := world(t, Options{})
	landThree(t, w)
	a := w.App()
	gh := &pausingGH{GitHub: &automerge.GH{Run: automerge.ExecRunner(a.Root)}, started: make(chan struct{}), proceed: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := a.NewAutomerge(gh).Check()
		done <- err
	}()
	<-gh.started
	w.MustSaddle("automerge", "hold", "t1")
	w.MustSaddle("automerge", "on")
	close(gh.proceed)
	must(t, <-done)

	st, err := w.App().AutomergeState()
	must(t, err)
	if !st.Enabled || !slices.Contains(st.Holds, "t1") {
		t.Fatalf("after the check: enabled=%v holds=%q; want on with t1 held", st.Enabled, st.Holds)
	}
}
