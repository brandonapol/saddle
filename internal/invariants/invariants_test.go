package invariants

import (
	"fmt"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 5, 13, 30, 0, 0, time.UTC)

// only asserts that s violates exactly one invariant, once, with kind and
// subject.
func only(t *testing.T, s Snapshot, kind Kind, subject string) Violation {
	t.Helper()
	vs := Check(s)
	if len(vs) != 1 || vs[0].Kind != kind || vs[0].Subject != subject || vs[0].Detail == "" {
		t.Fatalf("Check = %+v, want one %s on %s", vs, kind, subject)
	}
	return vs[0]
}

func none(t *testing.T, s Snapshot) {
	t.Helper()
	if vs := Check(s); len(vs) != 0 {
		t.Fatalf("Check = %+v, want none", vs)
	}
}

// I1, #819: seven tasks landed on the local train between 07:04 and 12:56
// and were reported done with no PR; nothing reached main.
func TestDoneNotOnMainFlagsPRlessLandedTasks(t *testing.T) {
	s := Snapshot{Now: now, Base: "main"}
	ids := []string{"t392", "t394", "t395", "t396", "t397", "t398", "t399", "t400"}
	for i, id := range ids {
		s.Landed = append(s.Landed, Landed{Task: id, LandedAt: now.Add(-time.Duration(6*60-i*40) * time.Minute)})
	}
	s.Landed[7].PR = "https://github.com/o/r/pull/816" // has a PR: on its way
	vs := Check(s)
	if len(vs) != 7 {
		t.Fatalf("Check = %+v, want 7 done_not_on_main", vs)
	}
	for i, v := range vs {
		if v.Kind != DoneNotOnMain || v.Subject != ids[i] {
			t.Fatalf("violation %d = %+v", i, v)
		}
	}
	// A task that landed five minutes ago is still within the grace.
	none(t, Snapshot{Now: now, Base: "main", Landed: []Landed{{Task: "t401", LandedAt: now.Add(-5 * time.Minute)}}})
	// One whose work is on base is done, PR or not.
	none(t, Snapshot{Now: now, Base: "main", Landed: []Landed{{Task: "t401", LandedAt: now.Add(-time.Hour), OnBase: true}}})
	// An unknown landing time is old, not young.
	only(t, Snapshot{Now: now, Base: "main", Landed: []Landed{{Task: "t401"}}}, DoneNotOnMain, "t401")
}

// I2: restack said it retargeted #814 but GitHub kept it where it was; and a
// PR the store says is open that GitHub no longer lists as open.
func TestPRDriftFlagsBaseAndStateTheStoreGotWrong(t *testing.T) {
	base := Snapshot{Now: now, Base: "main", PRsKnown: true}
	s := base
	s.PRs = []PR{{Task: "t397", URL: "#814", Branch: "saddle/t397", SavedBase: "saddle/t396", LiveBase: "main", Open: true}}
	s.Unmerged = map[string]string{"t397": "saddle/t397"}
	v := only(t, s, PRDrift, "t397")
	if v.Detail == "" {
		t.Fatal("no detail")
	}
	s.PRs[0].SavedBase = "main"
	none(t, s)
	s.PRs[0].Open = false
	only(t, s, PRDrift, "t397")
	// Without a live read this tick, nothing is compared.
	s.PRsKnown = false
	none(t, s)
}

// I3, #814: t397 was published on main while it used logErr from t396,
// whose PR (#812) hadn't merged.
func TestPRBaseWrongFlagsAPROnBaseAboveAnUnmergedDependency(t *testing.T) {
	s := Snapshot{Now: now, Base: "main", PRsKnown: true,
		Unmerged: map[string]string{"t396": "saddle/t396", "t397": "saddle/t397"},
		PRs: []PR{
			{Task: "t396", URL: "#812", Branch: "saddle/t396", LiveBase: "main", SavedBase: "main", Open: true},
			{Task: "t397", URL: "#814", Branch: "saddle/t397", LiveBase: "main", SavedBase: "main", Open: true, Deps: []string{"t396"}},
		}}
	only(t, s, PRBaseWrong, "t397")
	// On its dependency: right.
	s.PRs[1].LiveBase, s.PRs[1].SavedBase = "saddle/t396", "saddle/t396"
	none(t, s)
	// A base branch whose task merged is stale.
	delete(s.Unmerged, "t396")
	s.PRs = s.PRs[1:]
	only(t, s, PRBaseWrong, "t397")
	// The pre-publish build says its head doesn't build on base.
	s = Snapshot{Now: now, Base: "main", PRsKnown: true,
		PRs: []PR{{Task: "t398", URL: "#815", Branch: "saddle/t398", LiveBase: "main", Open: true, Builds: BuildsNo}}}
	only(t, s, PRBaseWrong, "t398")
	s.PRs[0].Builds = BuildsYes
	none(t, s)
}

// I4: an acked at-risk flag or a card whose default failed for good is a
// stop with nothing scheduled to retry it.
func TestLatchedFlagsStopsWithNoRetry(t *testing.T) {
	s := Snapshot{Now: now, Latches: []Latch{{What: "stack flag", Subject: "t396", Detail: "acked: conflict in prbase.go"}}}
	only(t, s, Latched, "stack flag t396")
	s.Latches[0].RetryAt = now.Add(time.Hour)
	none(t, s)
}

// I5, #784: a killed task set back to conflict by a train that raced the
// kill; and states with no way out.
func TestStateWithoutExitFlagsRevivedKillsAndStuckStates(t *testing.T) {
	s := Snapshot{Now: now, Tasks: []Task{{ID: "t390", Status: "conflict", Live: true, KilledAt: now.Add(-time.Hour)}}}
	only(t, s, StateWithoutExit, "task t390")
	// Requeued after the kill: an explicit way back.
	s.Tasks[0].ReturnedAt = now.Add(-30 * time.Minute)
	none(t, s)
	// Killed and still killed.
	none(t, Snapshot{Now: now, Tasks: []Task{{ID: "t390", Status: "killed", KilledAt: now.Add(-time.Hour)}}})

	only(t, Snapshot{Now: now, States: []State{{What: "hold", Subject: "t391"}}}, StateWithoutExit, "hold t391")
	past := Snapshot{Now: now, States: []State{{What: "card", Subject: "stuck:t1", Deadline: now.Add(-time.Hour)}}}
	only(t, past, StateWithoutExit, "card stuck:t1")
	// Just past its deadline: the tick that runs the exit may not have run.
	past.States[0].Deadline = now.Add(-time.Minute)
	none(t, past)
}

// I7, #820: ci-red repair spawned workers for saddle/base, a status saddle
// itself sets on every stacked PR.
func TestSelfRepairFlagsRepairsOfSaddlesOwnChecks(t *testing.T) {
	s := Snapshot{Now: now, Repairs: []Repair{{Repair: "t402", For: "t396", Checks: []string{"saddle/base"}}}}
	only(t, s, SelfRepair, "t402")
	s.Repairs[0].Checks = []string{"test"}
	none(t, s)
	s.Repairs[0].RedOnBase = []string{"test"}
	only(t, s, SelfRepair, "t402")
}

// I8: t402 and t403 were repairs spawned on grok, which the owner had taken
// out of [adapters] order.
func TestPolicyBypassFlagsSpawnsOutsideThePolicy(t *testing.T) {
	pol := Policy{Adapters: []string{"claude", "codex"}, ExcludeModels: map[string][]string{"codex": {"astra"}}, Serial: []string{"go.mod", "go.sum"}}
	s := Snapshot{Now: now, Policy: pol, Spawns: []Spawn{
		{Task: "t402", Adapter: "grok", Claims: []string{"internal/app/x.go"}},
		{Task: "t403", Adapter: "claude", Model: "opus"},
	}}
	only(t, s, PolicyBypass, "t402")
	s.Spawns = []Spawn{{Task: "t404", Adapter: "codex", Model: "gpt-6-astra"}}
	only(t, s, PolicyBypass, "t404")
	s.Spawns = []Spawn{{Task: "t405", Adapter: "claude", Claims: []string{"go.sum"}}}
	only(t, s, PolicyBypass, "t405")
	// An empty adapter list allows any adapter.
	s.Policy.Adapters, s.Spawns = nil, []Spawn{{Task: "t406", Adapter: "grok"}}
	none(t, s)
}

// Check is deterministic: the same snapshot gives the same violations in the
// same order.
func TestCheckIsSortedAndDeterministic(t *testing.T) {
	s := Snapshot{Now: now, Landed: []Landed{{Task: "t9"}, {Task: "t10"}}, Latches: []Latch{{What: "card", Subject: "c"}}}
	a, b := fmt.Sprint(Check(s)), fmt.Sprint(Check(s))
	if a != b {
		t.Fatalf("%s != %s", a, b)
	}
	vs := Check(s)
	for i := 1; i < len(vs); i++ {
		if vs[i-1].Key() > vs[i].Key() {
			t.Fatalf("unsorted: %+v", vs)
		}
	}
}

func TestDiffReportsNewAndCleared(t *testing.T) {
	a := Violation{Kind: Latched, Subject: "x", Detail: "d"}
	b := Violation{Kind: SelfRepair, Subject: "y", Detail: "d"}
	open := map[string]Violation{a.Key(): a}
	fresh, cleared := Diff(open, []Violation{a, b})
	if len(fresh) != 1 || fresh[0] != b || len(cleared) != 0 {
		t.Fatalf("Diff = %v, %v", fresh, cleared)
	}
	// A changed detail is the same violation: no new notice.
	a2 := a
	a2.Detail = "other"
	fresh, cleared = Diff(open, []Violation{a2})
	if len(fresh) != 0 || len(cleared) != 0 {
		t.Fatalf("Diff = %v, %v", fresh, cleared)
	}
	fresh, cleared = Diff(open, nil)
	if len(fresh) != 0 || len(cleared) != 1 || cleared[0] != a {
		t.Fatalf("Diff = %v, %v", fresh, cleared)
	}
}

// I5, #825: after an engine restart the monitor flagged ~380 train rows in
// terminal states. Terminal states have no exit by definition (ADR 0009);
// only a non-terminal row with no way out is a violation.
func TestStateWithoutExitSkipsTerminalTrainRows(t *testing.T) {
	var rows []State
	for i, st := range []string{"merged", "folded", "superseded"} {
		rows = append(rows, State{What: "train row", Subject: fmt.Sprintf("t%d", i), State: st, Detail: st + " for a task that is killed"})
	}
	none(t, Snapshot{Now: now, States: rows})

	// t96, t328: escalated for a killed task is real.
	rows = append(rows, State{What: "train row", Subject: "t96", State: "escalated", Detail: "escalated for a task that is killed"})
	only(t, Snapshot{Now: now, States: rows}, StateWithoutExit, "train row t96")
}
