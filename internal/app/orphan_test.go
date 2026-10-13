package app

import (
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/tmux"
)

// orphanedWorker spawns a worker that commits its work, calls done, and
// has that done call's gate cancelled: it commits once more while the gate
// runs, so the gate refuses the tree, and then it goes quiet at an idle
// prompt (t220, t221). The gate passes from then on.
func orphanedWorker(t *testing.T) (*App, *fakeTmux, store.Task) {
	t.Helper()
	a := trainSetup(t)
	ft := a.Tmux.(*fakeTmux)
	ft.timing = &tmux.Timing{RetryEvery: time.Millisecond, RetryFor: 20 * time.Millisecond, ConfirmEvery: time.Millisecond, ConfirmFor: 20 * time.Millisecond}
	cmd, runs, release := slowGate(t)
	a.Cfg.Train.Lint = config.Lint{Cmd: cmd, Set: true}
	tk, err := a.Spawn(SpawnReq{Title: "hung", Claims: []string{"x.go", "y.go"}})
	must(t, err)
	write(t, tk.Worktree, "x.go", "ok\n")
	commitAll(t, tk.Worktree, "work")
	if _, err := a.DoneAsync(tk.ID, "the hung work"); err != nil {
		t.Fatal(err)
	}
	if !waitUntil(func() bool { return runs() == 1 }) {
		t.Fatal("gate never started")
	}
	write(t, tk.Worktree, "y.go", "late\n")
	commitAll(t, tk.Worktree, "late commit")
	release()
	a.WaitDoneJobs()
	if inTrain(t, a, tk.ID) {
		t.Fatal("setup: the cancelled done queued the branch")
	}
	_, _ = a.Store.TakeNotices(tk.ID, false)
	ft.setScreen(tk.Window, "✻ Done.\n╭──────╮\n│ >    │\n╰──────╯")
	tk, err = a.Store.Task(tk.ID)
	must(t, err)
	return a, ft, tk
}

func openCardsOf(t *testing.T, a *App, kind string) []DecisionCard {
	t.Helper()
	cs, err := a.OpenDecisions()
	must(t, err)
	var out []DecisionCard
	for _, c := range cs {
		if c.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

// #467 acceptance: a worker whose done gate was cancelled and that then
// sits still is flagged done-orphaned, and saddle re-runs its gate itself,
// so the branch queues with no human step and no new done call.
func TestDoneOrphanIsFlaggedAndRegated(t *testing.T) {
	a, _, tk := orphanedWorker(t)
	now := time.Now().UTC()
	window := a.StallConfig().DoneOrphan
	if window != DefaultDoneOrphan {
		t.Fatalf("done_orphan default = %s", window)
	}

	if acted, err := a.OrphanTick(now); err != nil || len(acted) != 0 {
		t.Fatalf("first look: acted %v, %v", acted, err)
	}
	if acted, err := a.OrphanTick(now.Add(window / 2)); err != nil || len(acted) != 0 {
		t.Fatalf("before the window: acted %v, %v", acted, err)
	}
	if fl, _ := a.FlaggedOrphans(); len(fl) != 0 {
		t.Fatalf("flagged before the window: %+v", fl)
	}

	acted, err := a.OrphanTick(now.Add(window + time.Minute))
	must(t, err)
	if len(acted) != 1 || acted[0] != tk.ID {
		t.Fatalf("acted on %v, want %s", acted, tk.ID)
	}
	fl, err := a.FlaggedOrphans()
	must(t, err)
	if len(fl) != 1 || fl[0].Step != OrphanRegated {
		t.Fatalf("flagged = %+v, want %s re-gated", fl, tk.ID)
	}
	if cs := openCardsOf(t, a, DecisionDoneOrphaned); len(cs) != 1 || cs[0].Task != tk.ID {
		t.Fatalf("done-orphaned cards = %+v", cs)
	}
	h, err := a.Headline(now)
	must(t, err)
	if !strings.Contains(h.Text, tk.ID) || !strings.Contains(h.Text, "never queued") {
		t.Fatalf("headline %q doesn't name the orphan", h.Text)
	}

	a.WaitDoneJobs()
	if !inTrain(t, a, tk.ID) {
		t.Fatalf("re-run gate did not queue the branch; events: %v", orphanEvents(t, a, tk.ID))
	}
	got, err := a.Store.Task(tk.ID)
	must(t, err)
	if got.Summary != "the hung work" {
		t.Fatalf("summary = %q, want the done call's", got.Summary)
	}
	if _, err := a.OrphanTick(now.Add(window + 2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if cs := openCardsOf(t, a, DecisionDoneOrphaned); len(cs) != 0 {
		t.Fatalf("card still open after the branch queued: %+v", cs)
	}
	if os, _ := a.DoneOrphans(); len(os) != 0 {
		t.Fatalf("still watching: %+v", os)
	}
}

// #467: when the gate can't be re-run (the tree is dirty), saddle wakes the
// agent with a checked submit; when the session still doesn't move, it is
// killed and a finisher takes its commits.
func TestDoneOrphanNudgedThenFinished(t *testing.T) {
	a, ft, tk := orphanedWorker(t)
	write(t, tk.Worktree, "stray.txt", "left over\n")
	now := time.Now().UTC()
	window := a.StallConfig().DoneOrphan
	_, err := a.OrphanTick(now)
	must(t, err)

	_, err = a.OrphanTick(now.Add(window + time.Minute))
	must(t, err)
	fl, err := a.FlaggedOrphans()
	must(t, err)
	if len(fl) != 1 || fl[0].Step != OrphanNudged {
		t.Fatalf("flagged = %+v, want nudged", fl)
	}
	sent := ft.sentTo(tk.Window)
	if len(sent) == 0 || sent[len(sent)-1] != agent.WakeLine {
		t.Fatalf("typed %q, want the wake line", sent)
	}
	if n := noticeTexts(t, a, tk.ID); !strings.Contains(n, "call the saddle done tool again") {
		t.Fatalf("nudge notice = %q", n)
	}

	// The window runs out again with the screen as the nudge left it.
	_, err = a.OrphanTick(now.Add(2*window + 2*time.Minute))
	must(t, err)
	old, err := a.Store.Task(tk.ID)
	must(t, err)
	if old.Status != store.Killed {
		t.Fatalf("hung worker status = %s, want killed", old.Status)
	}
	ts, err := a.Store.Tasks()
	must(t, err)
	var fin store.Task
	for _, x := range ts {
		if strings.HasPrefix(x.Title, "finish "+tk.ID) {
			fin = x
		}
	}
	if fin.ID == "" {
		t.Fatalf("no finisher spawned; tasks: %+v", ts)
	}
	if !strings.Contains(fin.Prompt, "the hung work") || !strings.Contains(fin.Prompt, tk.Branch) {
		t.Fatalf("finisher prompt lacks the summary or branch:\n%s", fin.Prompt)
	}
	// Its branch starts with the hung worker's commits.
	if got := git(t, fin.Worktree, "log", "--format=%s", "-3"); !strings.Contains(got, "late commit") || !strings.Contains(got, "work") {
		t.Fatalf("finisher branch lacks the commits:\n%s", got)
	}
	cl, err := a.Store.Claims()
	must(t, err)
	if c := cl[fin.ID]; len(c) != 2 {
		t.Fatalf("finisher claims = %v, want the hung worker's", c)
	}
	if cs := openCardsOf(t, a, DecisionDoneOrphaned); len(cs) != 0 {
		t.Fatalf("card left open: %+v", cs)
	}
	if os, _ := a.DoneOrphans(); len(os) != 0 {
		t.Fatalf("still watching: %+v", os)
	}
}

// A screen that moves is an agent at work: it is never flagged.
func TestDoneOrphanMovingScreenIsLeftAlone(t *testing.T) {
	a, ft, tk := orphanedWorker(t)
	now := time.Now().UTC()
	window := a.StallConfig().DoneOrphan
	for i := range 4 {
		ft.setScreen(tk.Window, "working "+strings.Repeat(".", i))
		acted, err := a.OrphanTick(now.Add(time.Duration(i) * window))
		must(t, err)
		if len(acted) != 0 {
			t.Fatalf("tick %d acted on %v", i, acted)
		}
	}
	if fl, _ := a.FlaggedOrphans(); len(fl) != 0 {
		t.Fatalf("flagged a working agent: %+v", fl)
	}
}

// A done whose gate went red is the agent's to fix, not an orphan.
func TestDoneOrphanIgnoresRedGate(t *testing.T) {
	a := trainSetup(t)
	a.Cfg.Train.Lint = config.Lint{Cmd: badLint, Set: true}
	tk, err := a.Spawn(SpawnReq{Title: "red"})
	must(t, err)
	write(t, tk.Worktree, "x.go", "LINTBAD\n")
	commitAll(t, tk.Worktree, "bad")
	_, err = a.DoneAsync(tk.ID, "red")
	must(t, err)
	a.WaitDoneJobs()
	now := time.Now().UTC()
	for _, d := range []time.Duration{0, time.Hour, 2 * time.Hour} {
		if acted, err := a.OrphanTick(now.Add(d)); err != nil || len(acted) != 0 {
			t.Fatalf("acted %v, %v", acted, err)
		}
	}
}

func orphanEvents(t *testing.T, a *App, task string) []string {
	t.Helper()
	es, err := a.Store.Events(-1)
	must(t, err)
	var out []string
	for _, e := range es {
		if e.Task == task {
			out = append(out, e.Kind+" "+e.Data)
		}
	}
	return out
}
