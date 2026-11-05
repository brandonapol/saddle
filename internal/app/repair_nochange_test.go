package app

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

// endNoChange ends task r as a done task with nothing to land.
func endNoChange(t *testing.T, a *App, r, summary string) {
	t.Helper()
	must(t, a.Store.SetField(r, "summary", summary))
	must(t, a.Store.SetStatus(r, store.NoChange))
}

// #772: t1's repair ended no_change. It isn't live, so the original
// mustn't stay repairing with nothing working on it: saddle respawns the
// repair once, telling it what the last one said, and escalates when that
// one ends no_change too.
func TestRepairNoChangeUnblocksOriginal(t *testing.T) {
	t.Parallel()
	a, t1, r := orphanRepair(t)
	endNoChange(t, a, r.ID, "Nothing to do: the README conflict looked resolved.")
	if _, live := a.repairOf(t1.ID); live {
		t.Fatal("a no_change repair reads as live")
	}

	a.RetryRepairs()
	rs := repairTasks(t, a, t1.ID)
	if len(rs) != 2 {
		t.Fatalf("repair tasks after a no_change repair = %d, want a second one", len(rs))
	}
	r2 := rs[1]
	if r2.ID == r.ID {
		r2 = rs[0]
	}
	if !strings.Contains(r2.Prompt, r.ID) || !strings.Contains(r2.Prompt, "the README conflict looked resolved") {
		t.Fatalf("the respawned repair isn't told about %s's no_change:\n%s", r.ID, r2.Prompt)
	}

	endNoChange(t, a, r2.ID, "Still nothing to do.")
	_ = orchNotices(t, a, "")
	for range 2 {
		a.RetryRepairs()
		if _, err := a.Restack(); err != nil {
			t.Fatal(err)
		}
	}
	if rs := repairTasks(t, a, t1.ID); len(rs) != 2 {
		t.Fatalf("repair tasks after two no_change repairs = %d, want no third", len(rs))
	}
	if got := orchNotices(t, a, "nothing to land"); len(got) != 1 || !strings.Contains(got[0], "saddle unstack "+t1.ID) {
		t.Fatalf("escalation notices = %q, want one naming `saddle unstack %s`", got, t1.ID)
	}
	if a.autoRepairBlock(t1.ID) == "" {
		t.Fatal("no auto-repair block after two no_change repairs")
	}
}

// #772: a no_change repair whose original's work base already has: the
// original is merged, not repaired again, and its PR is closed.
func TestRepairNoChangeWorkOnBaseMergesOriginal(t *testing.T) {
	t.Parallel()
	a, t1, r := orphanRepair(t)
	t1, _ = a.Store.Task(t1.ID)
	l, ok := a.trainEntry(t1.ID)
	if !ok {
		t.Fatal("t1 has no train entry")
	}
	_, to, _ := strings.Cut(l.Note, "..")
	// Someone puts t1's work on main by hand.
	origin := strings.TrimSpace(git(t, a.Root, "remote", "get-url", "origin"))
	other := filepath.Join(t.TempDir(), "other")
	git(t, filepath.Dir(other), "clone", "-q", origin, other)
	git(t, other, "fetch", "-q", a.Root, to)
	git(t, other, "revert", "--no-edit", "HEAD")
	git(t, other, "cherry-pick", to)
	git(t, other, "push", "-q", "origin", "main")
	git(t, a.Root, "fetch", "-q", "origin")

	endNoChange(t, a, r.ID, "main already has it.")
	a.RetryRepairs()
	if rs := repairTasks(t, a, t1.ID); len(rs) != 1 {
		t.Fatalf("repair tasks = %d, want no respawn: base has t1's work", len(rs))
	}
	if got := trainState(t, a, t1.ID); got != TrainMerged {
		t.Fatalf("t1 train state = %q, want %s", got, TrainMerged)
	}
	if !hasEvent(t, a, "unstacked", "no_change") {
		t.Fatal("no unstacked event naming the no_change repair")
	}
}

// #772: saddle's own kills (orphan finisher, rescue) are not the owner
// declining a repair, and they don't use up CI repair attempts.
func TestInternalKillIsNotDecline(t *testing.T) {
	t.Parallel()
	a, t1, r := orphanRepair(t)
	if _, err := a.Rescue(r.ID); err != nil {
		t.Fatal(err)
	}
	if got := a.killedBy(r.ID); got != KilledByRescue {
		t.Fatalf("killedBy = %q, want %q", got, KilledByRescue)
	}
	if _, ok := a.repairDeclined(t1.ID); ok {
		t.Fatal("a rescue reads as the owner declining the repair")
	}
	a.RetryRepairs()
	if rs := repairTasks(t, a, t1.ID); len(rs) != 2 {
		t.Fatalf("repair tasks after a rescue = %d, want a respawn", len(rs))
	}

	// A kill by hand still declines.
	rs := repairTasks(t, a, t1.ID)
	r2 := rs[1]
	if r2.ID == r.ID {
		r2 = rs[0]
	}
	must(t, a.Kill(r2.ID, false))
	if got := a.killedBy(r2.ID); got != KilledByOwner {
		t.Fatalf("killedBy = %q, want %q", got, KilledByOwner)
	}
	if _, ok := a.repairDeclined(t1.ID); !ok {
		t.Fatal("an owner kill no longer declines")
	}
}

// #772: CI repair attempts count only the repairs that really tried: one
// saddle killed itself doesn't use one up.
func TestCIRepairAttemptsSkipInternalKills(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	s := CIRedState{Repairs: []CIRepairRecord{
		{Task: "t2", Head: "h1", Repair: "t5", State: ciRepairKilled},
		{Task: "t2", Head: "h1", Repair: "t6", State: ciRepairKilled},
		{Task: "t2", Head: "h2", Repair: "t7", State: ciRepairSpawned},
	}}
	a.Store.Event("t6", eventKilledBy, KilledByOrphan)
	if n := a.ciRepairAttempts(s, "t2"); n != 2 {
		t.Fatalf("attempts = %d, want 2 (t6 was killed by saddle)", n)
	}
}
