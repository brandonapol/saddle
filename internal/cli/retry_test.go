package cli

import (
	"slices"
	"strings"
	"testing"
)

// #936: with legacy_train off no repair watcher runs: no ci-red watcher
// (which spawns CI repairs), no CI watcher (which spawns CI fix tasks) and
// no old auto-merge watcher. Red CI and conflicts are the merge queue's
// fix-in-place pass. With legacy_train on the old set is unchanged.
func TestNoRepairWatcherWithLegacyTrainOff(t *testing.T) {
	a := automergeRepo(t)
	names := func() []string {
		var out []string
		for _, w := range watcherSet(a) {
			out = append(out, w.name)
		}
		return out
	}
	a.Cfg.LegacyTrain = false
	got := names()
	for _, bad := range []string{"ci-red", "ci", "automerge"} {
		if slices.Contains(got, bad) {
			t.Errorf("legacy_train off runs the %s watcher: %v", bad, got)
		}
	}
	if !slices.Contains(got, "queue") {
		t.Errorf("no merge queue watcher: %v", got)
	}
	a.Cfg.LegacyTrain = true
	if got := names(); !slices.Contains(got, "ci-red") || !slices.Contains(got, "automerge") {
		t.Errorf("legacy_train on lost its watchers: %v", got)
	}
}

// #936: saddle retry refuses a task with no PR to fix.
func TestRetryCmdRefusesTaskWithoutPR(t *testing.T) {
	automergeRepo(t)
	out, err := runCmd(t, Root(), "retry", "t1")
	if err == nil || !strings.Contains(err.Error()+out, "only a task with an open PR") {
		t.Fatalf("retry t1 = %v %q, want refused", err, out)
	}
}
