package app

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

// #292: task A claims dir/**, releases dir/f.go, and task B can then edit it.
// The release has to reach the claims table the write guard reads.
func TestReleaseOfPathUnderGlobFreesItForOthers(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	owner, err := a.Spawn(SpawnReq{Title: "billing", Claims: []string{"billing/**"}})
	must(t, err)
	other, err := a.Spawn(SpawnReq{Title: "meter fix"})
	must(t, err)
	f := filepath.Join(other.Worktree, "billing/meter.go")
	if d := a.CheckWrite(other.ID, f); d.Allow {
		t.Fatal("write under another task's glob allowed before release")
	}
	n, err := a.Store.ReleaseN(owner.ID, "billing/meter.go")
	must(t, err)
	if n != 1 {
		t.Fatalf("released %d claims, want 1", n)
	}
	if d := a.CheckWrite(other.ID, f); !d.Allow {
		t.Fatalf("released path still denied: %s", d.Reason)
	}
	// The rest of the glob stays the owner's, and the freed file is now other's.
	if d := a.CheckWrite(other.ID, filepath.Join(other.Worktree, "billing/invoice.go")); d.Allow {
		t.Fatal("unreleased sibling allowed")
	}
	if d := a.CheckWrite(owner.ID, filepath.Join(owner.Worktree, "billing/meter.go")); d.Allow {
		t.Fatal("owner still writes the file it released and other took")
	}
	// A plain claim on the released file no longer conflicts either.
	r, err := a.Claim(other.ID, []string{"billing/meter.go"})
	must(t, err)
	if len(r.Denied) != 0 {
		t.Fatalf("claim of released path denied: %v", r.Denied)
	}
}

// Re-claiming a path you released under your own glob takes it back.
func TestReclaimUndoesRelease(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	owner, err := a.Spawn(SpawnReq{Title: "billing", Claims: []string{"billing/**"}})
	must(t, err)
	other, err := a.Spawn(SpawnReq{Title: "other"})
	must(t, err)
	must(t, a.Store.Release(owner.ID, "billing/meter.go"))
	r, err := a.Claim(owner.ID, []string{"billing/meter.go"})
	must(t, err)
	if len(r.Denied) != 0 {
		t.Fatalf("reclaim denied: %v", r.Denied)
	}
	if d := a.CheckWrite(other.ID, filepath.Join(other.Worktree, "billing/meter.go")); d.Allow {
		t.Fatal("reclaimed path still free")
	}
}

// The denial names the owner and a self-service way to get the file.
func TestWriteDenialOffersSelfServicePath(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	owner, err := a.Spawn(SpawnReq{Title: "billing", Claims: []string{"billing/**"}})
	must(t, err)
	other, err := a.Spawn(SpawnReq{Title: "other"})
	must(t, err)
	d := a.CheckWrite(other.ID, filepath.Join(other.Worktree, "billing/meter.go"))
	if d.Allow {
		t.Fatal("allowed")
	}
	for _, want := range []string{owner.ID, "billing/**", "release", "task=" + owner.ID, "billing/meter.go", "saddle release --task " + owner.ID + " billing/meter.go"} {
		if !strings.Contains(d.Reason, want) {
			t.Fatalf("denial lacks %q:\n%s", want, d.Reason)
		}
	}
}

// #292: a CI repair may edit the files its red layer landed, even when a
// running task's glob claim covers them.
func TestCIRepairCarveOutForLandedLayerFiles(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	red := landTask(t, a, "t1", "shim", map[string]string{"runq/shim_test.go": "package runq\n"})
	holder, err := a.Spawn(SpawnReq{ID: "t2", Title: "runq rework", Claims: []string{"runq/**"}, Confirm: true})
	must(t, err)
	fix, err := a.Spawn(SpawnReq{ID: "t3", Title: ciFixTitle(red), Parent: OrchestratorID})
	must(t, err)
	f := filepath.Join(fix.Worktree, "runq/shim_test.go")
	if d := a.CheckWrite(fix.ID, f); d.Allow {
		t.Fatal("non-repair task allowed before it was recorded as a repair")
	}
	must(t, a.SetCIRed(CIRedState{
		Red:     []CIRedLayer{{Task: red.ID, Head: "abc", Checks: []string{"CI / test"}, Repair: fix.ID}},
		Repairs: []CIRepairRecord{{Task: red.ID, Head: "abc", Repair: fix.ID, State: ciRepairSpawned}},
	}))
	if d := a.CheckWrite(fix.ID, f); !d.Allow {
		t.Fatalf("CI repair denied its layer's landed file: %s", d.Reason)
	}
	// Files outside the layer are still the holder's.
	if d := a.CheckWrite(fix.ID, filepath.Join(fix.Worktree, "runq/other.go")); d.Allow || !strings.Contains(d.Reason, holder.ID) {
		t.Fatalf("repair allowed outside its layer: %+v", d)
	}
}

// #568: CIRedRepair records a repair only after Spawn returns, and the new
// agent may write before that. While its layer is red and no record names
// it yet, a task titled as that layer's repair is already its repair.
func TestCIRepairMayWriteBeforeItIsRecorded(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	red := landTask(t, a, "t1", "shim", map[string]string{"runq/shim.go": "package runq\n"})
	done, err := a.Spawn(SpawnReq{ID: "t2", Title: "runq follow-up", Claims: []string{"runq/**"}, Confirm: true})
	must(t, err)
	must(t, a.Store.SetStatus(done.ID, store.Done))
	must(t, a.SetCIRed(CIRedState{Red: []CIRedLayer{{Task: red.ID, Head: "abc", Checks: []string{"CI / test"}}}}))
	fix, err := a.Spawn(SpawnReq{ID: "t3", Title: ciFixTitle(red), Parent: OrchestratorID})
	must(t, err)
	f := filepath.Join(fix.Worktree, "runq/shim_test.go")
	if d := a.CheckWrite(fix.ID, f); !d.Allow {
		t.Fatalf("CI repair denied before its record was saved: %s", d.Reason)
	}

	// Once a record names it as killed, the title no longer counts.
	must(t, a.SetCIRed(CIRedState{
		Red:     []CIRedLayer{{Task: red.ID, Head: "abc", Checks: []string{"CI / test"}}},
		Repairs: []CIRepairRecord{{Task: red.ID, Head: "abc", Repair: fix.ID, State: ciRepairKilled}},
	}))
	if d := a.CheckWrite(fix.ID, filepath.Join(fix.Worktree, "runq/other_test.go")); d.Allow {
		t.Fatal("killed repair allowed under the done task's claim")
	}
}

// #466: a done task's claims guard queued work against conflicts, not a
// CI repair: the repair may write under them anywhere, while a running
// task's claim still blocks it outside the red layer.
func TestCIRepairMayWriteUnderDoneTaskClaim(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	red := landTask(t, a, "t1", "shim", map[string]string{"runq/shim.go": "package runq\n"})
	done, err := a.Spawn(SpawnReq{ID: "t2", Title: "runq follow-up", Claims: []string{"runq/**"}, Confirm: true})
	must(t, err)
	must(t, a.Store.SetStatus(done.ID, store.Done))
	running, err := a.Spawn(SpawnReq{ID: "t3", Title: "docs rework", Claims: []string{"docs/**"}})
	must(t, err)
	fix, err := a.Spawn(SpawnReq{ID: "t4", Title: ciFixTitle(red), Parent: OrchestratorID})
	must(t, err)
	f := filepath.Join(fix.Worktree, "runq/shim_test.go")
	if d := a.CheckWrite(fix.ID, f); d.Allow {
		t.Fatal("non-repair task allowed under the done task's claim")
	}
	must(t, a.SetCIRed(CIRedState{
		Red:     []CIRedLayer{{Task: red.ID, Head: "abc", Checks: []string{"CI / test"}, Repair: fix.ID}},
		Repairs: []CIRepairRecord{{Task: red.ID, Head: "abc", Repair: fix.ID, State: ciRepairSpawned}},
	}))
	if d := a.CheckWrite(fix.ID, f); !d.Allow {
		t.Fatalf("CI repair denied a file only done %s claims: %s", done.ID, d.Reason)
	}
	if d := a.CheckWrite(fix.ID, filepath.Join(fix.Worktree, "docs/x.md")); d.Allow || !strings.Contains(d.Reason, running.ID) {
		t.Fatalf("repair allowed under running %s's claim: %+v", running.ID, d)
	}
}
