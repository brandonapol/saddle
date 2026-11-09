package prgraph

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *SQLStore {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "state.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := OpenSQLStore(context.Background(), db, Policy{RepairAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The SQL store round-trips what Reconcile reads: changes, edges, attempts,
// and the host's policy.
func TestSQLStoreLoadRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTestDB(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	a := Change{ID: "c1", Branch: "b/c1", Base: "main", Head: "h1", Seq: 1, PR: 7, State: Published,
		Since: now, Deadline: now.Add(time.Hour), Automerge: true, Gen: 1}
	b := Change{ID: "c2", Branch: "b/c2", Base: "main", Head: "h2", Seq: 2, State: Queued,
		Reason: "", Since: now, HoldUntil: now.Add(2 * time.Hour), Gen: 1}
	for _, c := range []Change{b, a} {
		if err := s.PutChange(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutEdge(ctx, Edge{Change: "c2", Dep: "c1"}); err != nil {
		t.Fatal(err)
	}
	att := Attempt{Key: "merge/7/h1", Kind: "merge", Inputs: "h1", N: 2, Last: OutcomeRefused,
		NextAt: now.Add(time.Minute), FirstErr: "x", LastErr: "y", Escalated: true}
	if err := s.PutAttempt(ctx, att); err != nil {
		t.Fatal(err)
	}
	st, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Changes) != 2 || st.Changes[0].ID != "c1" || st.Changes[1].ID != "c2" {
		t.Fatalf("changes = %+v, want c1, c2 in id order", st.Changes)
	}
	if got := st.Changes[0]; got != a {
		t.Errorf("c1 = %+v, want %+v", got, a)
	}
	if got := st.Changes[1]; got != b {
		t.Errorf("c2 = %+v, want %+v", got, b)
	}
	if len(st.Edges) != 1 || st.Edges[0] != (Edge{Change: "c2", Dep: "c1"}) {
		t.Errorf("edges = %+v", st.Edges)
	}
	if len(st.Attempts) != 1 || st.Attempts[0] != att {
		t.Errorf("attempts = %+v, want %+v", st.Attempts, att)
	}
	if st.Policy.RepairAttempts != 2 {
		t.Errorf("policy = %+v, want the store's", st.Policy)
	}
}

// A Transition is a compare-and-set on gen: the stale one is (false, nil),
// and a dependency guard whose head moved makes it stale too.
func TestSQLStoreTransitionCAS(t *testing.T) {
	ctx := context.Background()
	s := openTestDB(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	for _, c := range []Change{
		{ID: "dep", Branch: "b/dep", Base: "main", Head: "hd", Seq: 1, State: Published, Since: now, Gen: 1},
		{ID: "c", Branch: "b/c", Base: "main", Head: "h1", Seq: 2, State: Queued, Since: now, Gen: 3},
	} {
		if err := s.PutChange(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	later := now.Add(time.Minute)
	tr := Action{Kind: ActTransition, Change: "c", To: Landed, Reason: "gate", Deadline: later.Add(30 * time.Minute),
		Guard: Guard{Gen: 3, Deps: []string{"dep=hd"}}}
	ok, err := s.Transition(ctx, tr, later)
	if err != nil || !ok {
		t.Fatalf("transition = %v, %v; want applied", ok, err)
	}
	if ok, err := s.Transition(ctx, tr, later); err != nil || ok {
		t.Fatalf("same gen again = %v, %v; want stale", ok, err)
	}
	st, _ := s.Load(ctx)
	c := st.Changes[0]
	if c.ID != "c" {
		c = st.Changes[1]
	}
	if c.State != Landed || c.Gen != 4 || !c.Since.Equal(later) || !c.Deadline.Equal(later.Add(30*time.Minute)) {
		t.Errorf("after transition c = %+v", c)
	}
	stale := Action{Kind: ActTransition, Change: "c", To: Published, Guard: Guard{Gen: 4, Deps: []string{"dep=other"}}}
	if ok, err := s.Transition(ctx, stale, later); err != nil || ok {
		t.Fatalf("dep guard on a moved head = %v, %v; want stale", ok, err)
	}
	// A transition to queued records the new head.
	nh := Action{Kind: ActTransition, Change: "c", To: Queued, Head: "h2", Guard: Guard{Gen: 4}}
	if ok, err := s.Transition(ctx, nh, later); err != nil || !ok {
		t.Fatalf("new head = %v, %v", ok, err)
	}
	st, _ = s.Load(ctx)
	for _, c := range st.Changes {
		if c.ID == "c" && c.Head != "h2" {
			t.Errorf("head = %q, want h2", c.Head)
		}
	}
}

// Recorded ticks come back in order and replay to the same plan.
func TestSQLStoreTicksReplay(t *testing.T) {
	ctx := context.Background()
	s := openTestDB(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	in := Input{Now: now, State: State{Changes: []Change{{ID: "c", Branch: "b/c", Base: "main", Head: "h", Seq: 1, State: Queued, Since: now, Gen: 1}}},
		Git: GitFacts{Bases: []BranchHead{{Branch: "main", SHA: "m1"}}, Heads: []BranchHead{{Branch: "b/c", SHA: "h"}}}}
	tk, err := NewTick(in, Reconcile(in))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTick(ctx, tk); err != nil {
		t.Fatal(err)
	}
	in2 := in
	in2.Now = now.Add(time.Minute)
	tk2, _ := NewTick(in2, Reconcile(in2))
	if err := s.RecordTick(ctx, tk2); err != nil {
		t.Fatal(err)
	}
	ts, err := s.Ticks(ctx, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 2 || !ts[0].TS.Equal(now) || !ts[1].TS.Equal(now.Add(time.Minute)) {
		t.Fatalf("ticks = %d, want 2 in order", len(ts))
	}
	for _, tk := range ts {
		if _, same, err := Replay(tk); err != nil || !same {
			t.Errorf("replay = %v, %v; want the recorded plan", same, err)
		}
	}
	n, err := s.PruneTicks(ctx, now.Add(30*time.Second))
	if err != nil || n != 1 {
		t.Errorf("prune = %d, %v; want 1", n, err)
	}
}
