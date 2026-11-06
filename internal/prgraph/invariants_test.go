package prgraph

// Property tests for ADR 0009 §4's invariants P1-P12. Each generates random
// DAGs of up to 50 changes with random snapshots and git facts, from a
// seed, so a failure names the seed that reproduces it.

import (
	"math/rand"
	"strings"
	"testing"
	"time"
)

// forSeeds runs prop on n seeds and stops at the first failure.
func forSeeds(t *testing.T, n int, prop func(t *testing.T, r *rand.Rand) bool) {
	t.Helper()
	if testing.Short() {
		n = max(n/10, 5)
	}
	for seed := int64(1); seed <= int64(n); seed++ {
		if !prop(t, rand.New(rand.NewSource(seed))) {
			t.Fatalf("property failed at seed %d", seed)
		}
	}
}

func actionsFor(p Plan, id string) []Action {
	var out []Action
	for _, a := range p.Actions {
		if a.Change == id {
			out = append(out, a)
		}
	}
	return out
}

// P1: Reconcile(x) equals Reconcile(x) and Reconcile(shuffle(x)), and it
// never changes its input.
func TestP1Determinism(t *testing.T) {
	t.Parallel()
	forSeeds(t, 400, func(t *testing.T, r *rand.Rand) bool {
		in := genWorld(r, genOpts{}).input()
		before := mustJSON(in)
		a := mustJSON(Reconcile(in))
		if mustJSON(in) != before {
			t.Errorf("Reconcile changed its input")
			return false
		}
		if b := mustJSON(Reconcile(in)); a != b {
			t.Errorf("two runs differ:\n%s\n%s", a, b)
			return false
		}
		if c := mustJSON(Reconcile(shuffled(r, in))); a != c {
			t.Errorf("shuffled input gives another plan:\n%s\n%s", a, c)
			return false
		}
		return true
	})
}

// P1: a recorded tick replays to the same plan.
func TestP1Replay(t *testing.T) {
	t.Parallel()
	forSeeds(t, 100, func(t *testing.T, r *rand.Rand) bool {
		in := genWorld(r, genOpts{}).input()
		tk, err := NewTick(in, Reconcile(in))
		if err != nil {
			t.Fatal(err)
		}
		_, same, err := Replay(tk)
		if err != nil || !same {
			t.Errorf("replay differs (err %v)", err)
			return false
		}
		return true
	})
}

// P2: every state in the table has a deadline and an automatic exit, and
// the exit is an action.
func TestP2StateTable(t *testing.T) {
	t.Parallel()
	seen := map[ChangeState]bool{}
	kinds := map[ActionKind]bool{ActGate: true, ActRebase: true, ActPublish: true, ActMove: true, ActLink: true,
		ActMerge: true, ActNeedsFix: true, ActClose: true, ActTransition: true, ActEscalate: true}
	for _, rule := range StateTable() {
		seen[rule.State] = true
		if rule.State.Terminal() {
			continue
		}
		if rule.Deadline == nil || rule.Deadline(Policy{}) <= 0 {
			t.Errorf("%s has no deadline", rule.State)
		}
		if len(rule.Exits) == 0 {
			t.Errorf("%s has no exit", rule.State)
		}
		for _, e := range rule.Exits {
			if !kinds[e.By] {
				t.Errorf("%s -> %s exits by %q, not an action", rule.State, e.To, e.By)
			}
		}
	}
	for _, s := range []ChangeState{Queued, Landed, Published, Green, Red, Merged, Superseded} {
		if !seen[s] {
			t.Errorf("state %s missing from the table", s)
		}
	}
}

// P2: every non-terminal change has a next action: an effectful one, or a
// Wait with a time or on another change.
func TestP2EveryChangeHasANextAction(t *testing.T) {
	t.Parallel()
	forSeeds(t, 400, func(t *testing.T, r *rand.Rand) bool {
		w := genWorld(r, genOpts{})
		p := Reconcile(w.input())
		for _, c := range w.st.Changes {
			if c.State.Terminal() {
				continue
			}
			as := actionsFor(p, c.ID)
			if len(as) == 0 {
				t.Errorf("%s (%s) has no action", c.ID, c.State)
				return false
			}
			for _, a := range as {
				if a.Kind == ActWait && a.Until.IsZero() && (a.Blocker == "" || a.Blocker == c.ID) {
					t.Errorf("%s waits with no time and no blocker: %v", c.ID, a)
					return false
				}
			}
		}
		return true
	})
}

// P2: in a silent world (writes succeed, CI never reports, nobody pushes),
// advancing Now takes every change to merged or superseded within a bound
// computed from the DAG's depth.
func TestP2SilentWorldTerminates(t *testing.T) {
	t.Parallel()
	forSeeds(t, 60, func(t *testing.T, r *rand.Rand) bool {
		w := genWorld(r, genOpts{clean: true, maxN: 15})
		w.ci = ciSilent
		p := w.st.Policy.withDefaults()
		per := p.QueuedDeadline + p.LandedDeadline + p.PublishedDeadline + p.GreenDeadline +
			time.Duration(p.RepairAttempts+1)*(p.FixWait+p.RetryMax) + time.Hour
		bound := time.Duration(depth(w)+1) * per
		step := 10 * time.Minute
		for w.now.Sub(t0) <= bound {
			w.tick()
			if allTerminal(w) {
				return true
			}
			w.now = w.now.Add(step)
		}
		for _, c := range w.st.Changes {
			if !c.State.Terminal() {
				t.Errorf("%s still %s(%s) after %s", c.ID, c.State, c.Reason, bound)
			}
		}
		return false
	})
}

func allTerminal(w *world) bool {
	for _, c := range w.st.Changes {
		if !c.State.Terminal() {
			return false
		}
	}
	return true
}

// P3: a red change holds only itself and the changes with a path of edges
// to it. Making one change red changes no other change's actions unless
// that change depends on it.
func TestP3RedHoldsOnlyDependents(t *testing.T) {
	t.Parallel()
	forSeeds(t, 300, func(t *testing.T, r *rand.Rand) bool {
		w := genWorld(r, genOpts{})
		p := Reconcile(w.input())
		for _, a := range p.Actions {
			if a.Kind == ActWait && a.Blocker != "" && a.Blocker != a.Change && !oraclePath(w, a.Change, a.Blocker) {
				t.Errorf("%v waits on %s, which it doesn't depend on", a, a.Blocker)
				return false
			}
		}
		x := w.st.Changes[r.Intn(len(w.st.Changes))]
		if x.State.Terminal() || w.inBase[x.ID] {
			return true
		}
		red := w.clone()
		rc := red.change(x.ID)
		rc.State, rc.Reason = Red, CheckCause("ci")
		red.setCheck(Check{Head: red.heads[rc.Branch], Name: "ci", Status: CheckFailure, Required: true})
		q := Reconcile(red.input())
		for _, c := range w.st.Changes {
			if c.ID == x.ID || oraclePath(w, c.ID, x.ID) {
				continue
			}
			if a, b := mustJSON(actionsFor(p, c.ID)), mustJSON(actionsFor(q, c.ID)); a != b {
				t.Errorf("%s red changed %s's actions:\n%s\n%s", x.ID, c.ID, a, b)
				return false
			}
		}
		return true
	})
}

// P4: every Gate, Rebase and Publish targets base tip plus the heads of the
// change's unmerged dependencies, and nothing else. No action names the
// integration branch.
func TestP4TargetsBasePlusDeps(t *testing.T) {
	t.Parallel()
	forSeeds(t, 400, func(t *testing.T, r *rand.Rand) bool {
		w := genWorld(r, genOpts{})
		p := Reconcile(w.input())
		for _, a := range p.Actions {
			if strings.Contains(mustJSON(a), "integration") {
				t.Errorf("%v names the integration branch", a)
				return false
			}
			switch a.Kind {
			case ActGate, ActRebase:
				if want := oracleOnto(w, a.Change); a.Onto == nil || a.Onto.Key() != want.Key() {
					t.Errorf("%v: want onto %s", a, want.Key())
					return false
				}
			case ActPublish:
				if want, _ := oracleDesiredBase(w, a.Change); a.Base != want {
					t.Errorf("%v: want base %s", a, want)
					return false
				}
			}
		}
		return true
	})
}

// P5: when a required check is red on base's tip, no change gets NeedsFix
// for it; exactly one NeedsFix(base, check) and one Escalate per (base tip,
// check).
func TestP5BaseRed(t *testing.T) {
	t.Parallel()
	forSeeds(t, 300, func(t *testing.T, r *rand.Rand) bool {
		w := genWorld(r, genOpts{baseRed: true})
		live := false
		for _, c := range w.st.Changes {
			live = live || (!c.State.Terminal() && !w.inBase[c.ID])
		}
		p := Reconcile(w.input())
		var baseFix, esc int
		for _, a := range p.Actions {
			if a.Kind == ActNeedsFix && a.Change != "" && a.Cause == CheckCause("ci") {
				t.Errorf("%v repairs a base-red check", a)
				return false
			}
			if a.Kind == ActNeedsFix && a.Change == "" && a.Base == "main" && a.Cause == CheckCause("ci") {
				baseFix++
			}
			if a.Kind == ActEscalate && strings.Contains(a.Key, "tip1") && a.Cause == CheckCause("ci") {
				esc++
			}
			if a.Kind == ActNeedsFix && a.Change == "" && a.Cause == CheckCause("saddle/base") {
				t.Errorf("%v repairs a host check", a)
				return false
			}
		}
		if live && (baseFix != 1 || esc != 1) {
			t.Errorf("base red: %d base fixes and %d escalations, want 1 and 1", baseFix, esc)
			return false
		}
		// Once applied, neither comes again for the same tip.
		ctx := w.engine()
		for _, a := range p.Actions {
			if a.Change == "" && (a.Kind == ActNeedsFix || a.Kind == ActEscalate) {
				if _, err := ctx.apply(t.Context(), w.input(), a); err != nil {
					t.Fatal(err)
				}
			}
		}
		for _, a := range Reconcile(w.input()).Actions {
			if a.Change == "" && (a.Kind == ActNeedsFix || a.Kind == ActEscalate) {
				t.Errorf("%v repeated for the same base tip", a)
				return false
			}
		}
		return true
	})
}

// P6: NeedsFix causes are never a host-owned status, and never a red that
// a Move explains (the change's own PR, or one below it, is on the wrong
// base).
func TestP6NeedsFixOnlyForOwnRed(t *testing.T) {
	t.Parallel()
	forSeeds(t, 400, func(t *testing.T, r *rand.Rand) bool {
		w := genWorld(r, genOpts{})
		p := Reconcile(w.input())
		wrongBase := map[string]bool{}
		for _, c := range w.st.Changes {
			pr := w.openPR(c.Branch)
			if pr == nil || c.State.Terminal() || c.State == Queued || oracleMerged(w, c.ID) {
				continue
			}
			if want, ok := oracleDesiredBase(w, c.ID); ok && pr.Base != want {
				wrongBase[c.ID] = true
			}
		}
		for _, a := range p.Actions {
			if a.Kind != ActNeedsFix {
				continue
			}
			for _, h := range w.st.Policy.HostChecks {
				if a.Cause == CheckCause(h) {
					t.Errorf("%v repairs a host check", a)
					return false
				}
			}
			if a.Change == "" {
				continue
			}
			for id := range wrongBase {
				if id == a.Change || oraclePath(w, a.Change, id) {
					t.Errorf("%v, but %s is on the wrong base: that is a Move", a, id)
					return false
				}
			}
		}
		return true
	})
}

// P7: with only transient or rate-limited outcomes (and a degraded forge),
// there is no Transition to red or superseded, no Escalate, and no clock
// moves a deadline it already had.
func TestP7ForgeWaitsNeverFail(t *testing.T) {
	t.Parallel()
	forSeeds(t, 40, func(t *testing.T, r *rand.Rand) bool {
		w := genWorld(r, genOpts{noFailing: true})
		w.degraded = true
		w.outcome = func(a Action) Outcome {
			if r.Intn(2) == 0 {
				return Outcome{Kind: OutcomeTransient, Reason: "502"}
			}
			return Outcome{Kind: OutcomeRateLimited, Until: w.now.Add(10 * time.Minute)}
		}
		for range 120 {
			before := map[string]Change{}
			for _, c := range w.st.Changes {
				before[c.ID] = c
			}
			plan := Reconcile(w.input())
			for _, a := range plan.Actions {
				if a.Kind == ActEscalate {
					t.Errorf("escalated on forge waits: %v", a)
					return false
				}
				if a.Kind == ActTransition && (a.To == Red || a.To == Superseded) && !w.inBase[a.Change] {
					t.Errorf("forge waits made %v", a)
					return false
				}
				if b := before[a.Change]; a.Kind == ActTransition && a.Head == "" && a.To == b.State && !b.Deadline.IsZero() && !a.Deadline.Equal(b.Deadline) {
					t.Errorf("forge waits moved %s's deadline: %v", a.Change, a)
					return false
				}
			}
			w.tick()
			w.now = w.now.Add(10 * time.Minute)
		}
		return true
	})
}

// P8: applying plans to the model world reaches a fixpoint where the plan
// is only waits. Crashing after any prefix of the first plan and then
// reconciling reaches the same fixpoint. Every effectful action has a guard.
func TestP8FixpointAndCrashPrefix(t *testing.T) {
	t.Parallel()
	forSeeds(t, 60, func(t *testing.T, r *rand.Rand) bool {
		w := genWorld(r, genOpts{clean: r.Intn(2) == 0})
		w.ci = []ciMode{ciFixed, ciGreen}[r.Intn(2)]
		first := Reconcile(w.input())
		for _, a := range first.Effectful() {
			if a.Guard.IsZero() {
				t.Errorf("%v has no guard", a)
				return false
			}
		}
		ref := w.clone()
		if !fixpoint(t, ref) {
			return false
		}
		want := worldShape(ref)
		eff := first.Effectful()
		// Every prefix length on small plans; a sample of them on big ones.
		ks := []int{}
		for k := 0; k <= len(eff); k++ {
			if len(eff) <= 8 || k <= 2 || k == len(eff) || r.Intn(len(eff)) < 4 {
				ks = append(ks, k)
			}
		}
		for _, k := range ks {
			c := w.clone()
			e := c.engine()
			for _, a := range eff[:k] {
				if _, err := e.apply(t.Context(), c.input(), a); err != nil {
					t.Fatal(err)
				}
			}
			if !fixpoint(t, c) {
				return false
			}
			if got := worldShape(c); got != want {
				t.Errorf("crash after %d of %d actions reaches another fixpoint:\n%s\n%s", k, len(eff), got, want)
				return false
			}
		}
		// P10 at the fixpoint: every open PR is on its desired base.
		for _, c := range ref.st.Changes {
			pr := ref.openPR(c.Branch)
			if pr == nil || c.State.Terminal() || c.State == Queued || c.HoldUntil.After(ref.now) || c.RetryAt.After(ref.now) {
				continue // queued changes aren't moved yet; held ones wait
			}
			if want, ok := oracleDesiredBase(ref, c.ID); ok && pr.Base != want {
				t.Errorf("fixpoint: %s's PR on %s, want %s", c.ID, pr.Base, want)
				return false
			}
		}
		return true
	})
}

func fixpoint(t *testing.T, w *world) bool {
	t.Helper()
	w.tick()
	p := Reconcile(w.input())
	if eff := p.Effectful(); len(eff) > 0 {
		t.Errorf("no fixpoint; still %v", eff)
		return false
	}
	return true
}

// worldShape is the world without bookkeeping that legitimately differs
// between paths (row gens, PR numbers, attempt rows).
func worldShape(w *world) string {
	type row struct {
		ID, State, Reason, Head, PRBase, PRState string
		Deadline, RetryAt                        time.Time
	}
	var rows []row
	for _, c := range w.st.Changes {
		x := row{ID: c.ID, State: string(c.State), Reason: c.Reason, Head: c.Head, Deadline: c.Deadline, RetryAt: c.RetryAt}
		if p := w.openPR(c.Branch); p != nil {
			x.PRBase, x.PRState = p.Base, string(p.State)
		}
		rows = append(rows, x)
	}
	return mustJSON(struct {
		Rows   []row
		InBase []string
		Bases  map[string]string
	}{rows, sortedKeys(w.inBase), w.bases})
}

// P9: Merge of a change only when all its dependencies are merged; a change
// whose deps are merged and is otherwise ready gets its Merge; merges in a
// cooperative world happen in topological order.
func TestP9MergeOrder(t *testing.T) {
	t.Parallel()
	forSeeds(t, 300, func(t *testing.T, r *rand.Rand) bool {
		w := genWorld(r, genOpts{})
		for _, a := range Reconcile(w.input()).Actions {
			if a.Kind != ActMerge {
				continue
			}
			if u := oracleUnmerged(w, a.Change); len(u) > 0 {
				t.Errorf("%v while deps %v are unmerged", a, u)
				return false
			}
		}
		return true
	})
	forSeeds(t, 60, func(t *testing.T, r *rand.Rand) bool {
		w := genWorld(r, genOpts{clean: true, maxN: 15})
		w.ci = ciGreen
		w.native = true
		for i := range w.st.Changes {
			w.st.Changes[i].Automerge = true
		}
		for range 40 {
			w.tick()
			if allTerminal(w) {
				break
			}
			w.now = w.now.Add(10 * time.Minute)
		}
		merged := map[string]bool{}
		for _, ap := range w.applied {
			if ap.Action.Kind != ActMerge || ap.Outcome.Kind != OutcomeOK {
				continue
			}
			for _, d := range oracleDeps(w, ap.Action.Change) {
				if !merged[d] {
					t.Errorf("%s merged before its dep %s", ap.Action.Change, d)
					return false
				}
			}
			merged[ap.Action.Change] = true
		}
		for _, c := range w.st.Changes {
			if c.State != Merged {
				t.Errorf("cooperative world: %s ended %s(%s)", c.ID, c.State, c.Reason)
				return false
			}
		}
		return true
	})
}

// P10: each published PR's desired base is its nearest unmerged
// dependency, else base. A PR on another base gets a Move there.
func TestP10DesiredBase(t *testing.T) {
	t.Parallel()
	forSeeds(t, 400, func(t *testing.T, r *rand.Rand) bool {
		w := genWorld(r, genOpts{})
		p := Reconcile(w.input())
		for _, a := range p.Actions {
			if a.Kind == ActMove {
				if want, ok := oracleDesiredBase(w, a.Change); !ok || a.Base != want {
					t.Errorf("%v: want base %q (one chain: %v)", a, want, ok)
					return false
				}
			}
		}
		for _, c := range w.st.Changes {
			pr := w.openPR(c.Branch)
			if pr == nil || c.State.Terminal() || c.State == Queued || oracleMerged(w, c.ID) || w.heads[c.Branch] != c.Head {
				continue
			}
			want, ok := oracleDesiredBase(w, c.ID)
			if !ok || pr.Base == want || w.degraded || c.HoldUntil.After(w.now) {
				continue
			}
			moved := false
			for _, a := range actionsFor(p, c.ID) {
				moved = moved || (a.Kind == ActMove && a.PR == pr.Number) ||
					(a.Kind == ActWait && strings.HasPrefix(a.Reason, "retry"))
			}
			if !moved {
				t.Errorf("%s's PR #%d is on %s, want %s; plan %v", c.ID, pr.Number, pr.Base, want, actionsFor(p, c.ID))
				return false
			}
		}
		return true
	})
}

// P11: no Transition out of merged or superseded (a requeue is the host's
// write, not Reconcile's), except that a change whose work is in base is
// merged, whatever its row says.
func TestP11TerminalStaysTerminal(t *testing.T) {
	t.Parallel()
	forSeeds(t, 400, func(t *testing.T, r *rand.Rand) bool {
		w := genWorld(r, genOpts{})
		p := Reconcile(w.input())
		for _, c := range w.st.Changes {
			var to []ChangeState
			for _, a := range actionsFor(p, c.ID) {
				if a.Kind == ActTransition {
					to = append(to, a.To)
				}
			}
			if w.inBase[c.ID] && c.State != Merged && !contains(stateStrings(to), string(Merged)) {
				t.Errorf("%s is in base but stays %s (transitions %v)", c.ID, c.State, to)
				return false
			}
			if !c.State.Terminal() {
				continue
			}
			for _, s := range to {
				if c.State != Superseded || s != Merged || !w.inBase[c.ID] {
					t.Errorf("%s left terminal %s for %s", c.ID, c.State, s)
					return false
				}
			}
		}
		return true
	})
}

func stateStrings(ss []ChangeState) []string {
	var out []string
	for _, s := range ss {
		out = append(out, string(s))
	}
	return out
}

// P12: Collapsible returns only green, dependency-closed groups, and it
// never changes the plan.
func TestP12CollapseIsAView(t *testing.T) {
	t.Parallel()
	forSeeds(t, 400, func(t *testing.T, r *rand.Rand) bool {
		w := genWorld(r, genOpts{})
		in := w.input()
		before := mustJSON(Reconcile(in))
		groups := Collapsible(in)
		if mustJSON(Reconcile(in)) != before {
			t.Errorf("Collapsible changed the plan")
			return false
		}
		for _, g := range groups {
			set := map[string]bool{}
			for _, id := range g.Changes {
				set[id] = true
			}
			if !set[g.Root] {
				t.Errorf("group %v lacks its root", g)
				return false
			}
			for _, id := range g.Changes {
				c := w.change(id)
				if c == nil || c.State != Green || oracleMerged(w, id) {
					t.Errorf("group %v holds %s, which isn't green", g, id)
					return false
				}
				for _, d := range oracleUnmerged(w, id) {
					if !set[d] {
						t.Errorf("group %v holds %s but not its dep %s", g, id, d)
						return false
					}
				}
			}
		}
		return true
	})
}
