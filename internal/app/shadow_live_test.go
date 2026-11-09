package app

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/forge"
	"github.com/brandonapol/saddle/internal/prgraph"
	"github.com/brandonapol/saddle/internal/store"
)

// fakeShadowSource is a read-only source; it counts reads.
type fakeShadowSource struct {
	snap  prgraph.ForgeSnapshot
	git   prgraph.GitFacts
	reads int
}

func (f *fakeShadowSource) Snapshot(context.Context, []prgraph.ChangeRef) (prgraph.ForgeSnapshot, error) {
	f.reads++
	return f.snap, nil
}

func (f *fakeShadowSource) GitFacts(context.Context, []prgraph.ChangeRef) (prgraph.GitFacts, error) {
	return f.git, nil
}

// stateFiles lists .saddle's files with their sizes and times, so a test
// can see that nothing but state.db changed.
func stateFiles(t *testing.T, a *App) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.Walk(a.stateDir(), func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || strings.HasPrefix(filepath.Base(p), "state.db") {
			return nil
		}
		out[p] = info.ModTime().String() + "/" + strconv.FormatInt(info.Size(), 10)
		return nil
	})
	return out
}

// The shadow watcher logs reconcile_shadow events for each divergence:
// t396's host/order red got a legacy repair prgraph wouldn't give, and
// t397's real red gets a fix legacy never asked for. It acts on nothing:
// no state file changes, and its source is only read.
func TestShadowReconcilerLogsDivergencesAndNeverActs(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	for _, tk := range []store.Task{
		{ID: "t396", Title: "governor", Role: store.RoleWorker, Branch: "saddle/t396-gov", Status: store.Landed, PR: "https://github.com/o/r/pull/812"},
		{ID: "t397", Title: "autopilot", Role: store.RoleWorker, Branch: "saddle/t397-auto", Status: store.Landed, PR: "https://github.com/o/r/pull/814"},
	} {
		must(t, a.Store.CreateTask(tk))
		must(t, a.Store.Enqueue(tk.ID))
		must(t, a.Store.SetTrain(tk.ID, store.TrainOK, "base..h-"+tk.ID, false))
	}
	src := &fakeShadowSource{
		snap: prgraph.ForgeSnapshot{
			PRs: []prgraph.PR{
				{Number: 812, Branch: "saddle/t396-gov", Head: "h-t396", Base: "main", State: prgraph.PROpen, Mergeable: prgraph.MergeClean},
				{Number: 814, Branch: "saddle/t397-auto", Head: "h-t397", Base: "main", State: prgraph.PROpen, Mergeable: prgraph.MergeClean},
			},
			Checks: []prgraph.Check{
				{Head: "h-t396", Name: "host/order", Status: prgraph.CheckFailure, Required: true, HostOwned: true},
				{Head: "h-t396", Name: "ci / test", Status: prgraph.CheckPending, Required: true},
				{Head: "h-t397", Name: "ci / lint", Status: prgraph.CheckFailure, Required: true},
			},
		},
		git: prgraph.GitFacts{Bases: []prgraph.BranchHead{{Branch: "main", SHA: "m1"}},
			Heads: []prgraph.BranchHead{{Branch: "saddle/t396-gov", SHA: "h-t396"}, {Branch: "saddle/t397-auto", SHA: "h-t397"}}},
	}
	now := shadowNow
	r := &ShadowReconciler{App: a, Source: src, Clock: func() time.Time { return now }}
	before := stateFiles(t, a)
	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.Store.Event("t396", "ci_repair_spawned", "t402")
	now = now.Add(2 * time.Minute)
	out, err := r.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Side != ShadowLegacyOnly || out[0].Change != "t396" || out[0].Kind != prgraph.ActNeedsFix {
		t.Fatalf("second tick = %+v; want legacy_only needs_fix(t396)", out)
	}
	now = now.Add(ShadowWindow)
	out, err = r.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range out {
		if d.Side == ShadowPrgraphOnly && d.Change == "t397" && d.Kind == prgraph.ActNeedsFix {
			found = true
		}
	}
	if !found {
		t.Fatalf("after the window = %+v; want prgraph_only needs_fix(t397)", out)
	}
	evs, _ := a.Store.Events(-1)
	var lines []string
	for _, e := range evs {
		if e.Kind == EventReconcileShadow {
			lines = append(lines, e.Task+": "+e.Data)
		}
	}
	if len(lines) < 2 || !strings.Contains(strings.Join(lines, "\n"), "legacy_only needs_fix(t396") {
		t.Errorf("reconcile_shadow events = %q", lines)
	}
	if after := stateFiles(t, a); len(after) != len(before) {
		t.Errorf("the shadow changed state files:\nbefore %v\nafter  %v", before, after)
	} else {
		for p, v := range before {
			if after[p] != v {
				t.Errorf("the shadow changed %s", p)
			}
		}
	}
	if src.reads != 3 {
		t.Errorf("forge snapshots = %d, want one per tick", src.reads)
	}
}

// The forge's check rollup maps onto prgraph's checks, statuses included.
// Saddle posts no statuses of its own (ADR 0012), so none is host-owned.
func TestForgeSnapshotChecks(t *testing.T) {
	t.Parallel()
	snap := forgeSnapshot([]forge.PR{{Number: 5, Head: "b", HeadSHA: "h", Base: "main", State: forge.StateOpen, Mergeable: forge.Conflicting,
		Checks: []forge.CheckRun{
			{Kind: forge.KindJob, Workflow: "ci", Name: "lint", Status: "COMPLETED", Conclusion: "FAILURE"},
			{Kind: forge.KindJob, Workflow: "ci", Name: "test", Status: "IN_PROGRESS"},
			{Kind: forge.KindStatus, Context: "host/order", State: "SUCCESS"},
		}}})
	if len(snap.PRs) != 1 || snap.PRs[0].Mergeable != prgraph.MergeConflicting || snap.PRs[0].Branch != "b" {
		t.Fatalf("prs = %+v", snap.PRs)
	}
	want := map[string]prgraph.CheckStatus{"ci / lint": prgraph.CheckFailure, "ci / test": prgraph.CheckPending, "host/order": prgraph.CheckSuccess}
	for _, c := range snap.Checks {
		if want[c.Name] != c.Status {
			t.Errorf("%s = %s, want %s", c.Name, c.Status, want[c.Name])
		}
		if c.HostOwned {
			t.Errorf("%s host-owned = %v", c.Name, c.HostOwned)
		}
	}
}

func TestShadowEdgesFromDependsOn(t *testing.T) {
	t.Parallel()
	got := shadowEdges("t5", "t1,t2")
	if len(got) != 2 || got[1].Dep != "t2" || got[0].Kind != prgraph.Declared {
		t.Errorf("spawn edges = %+v", got)
	}
	if e := shadowEdges("t396", "t395 (inferred: t396's task text names t395)"); len(e) != 1 || e[0].Kind != prgraph.Inferred || e[0].Dep != "t395" {
		t.Errorf("inferred = %+v", e)
	}
	if e := shadowEdges("t6", "t4 (its PR was retargeted onto t4's branch)"); len(e) != 1 || e[0].Source != "hand_retarget" {
		t.Errorf("retarget = %+v", e)
	}
}

// The live shadow records a tick in prgraph_tick when its input changes,
// not on every tick, and the recorded tick replays to the same plan.
func TestShadowReconcilerRecordsChangedTicks(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	must(t, a.Store.CreateTask(store.Task{ID: "t1", Title: "x", Role: store.RoleWorker, Branch: "saddle/t1-x", Status: store.Done}))
	must(t, a.Store.Enqueue("t1"))
	src := &fakeShadowSource{git: prgraph.GitFacts{Bases: []prgraph.BranchHead{{Branch: "main", SHA: "m1"}}}}
	now := shadowNow
	r := a.NewShadowReconciler()
	defer r.Ticks.DB.Close()
	r.Source, r.Clock = src, func() time.Time { return now }
	for range 3 {
		if _, err := r.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
	}
	src.git.Bases[0].SHA = "m2"
	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	ts, err := r.Ticks.Ticks(context.Background(), shadowNow, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 2 {
		t.Fatalf("recorded %d ticks, want 2 (one per changed input)", len(ts))
	}
	if _, same, err := prgraph.Replay(ts[1]); err != nil || !same {
		t.Errorf("replay = %v, %v", same, err)
	}
}
