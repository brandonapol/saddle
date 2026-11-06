package prgraph

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// Wait reasons Reconcile gives.
const (
	WaitHold          = "hold"
	WaitRetry         = "retry"
	WaitForge         = "forge_degraded"
	WaitDepsNotChain  = "deps_not_in_one_chain"
	WaitDepsUnmerged  = "deps_unmerged"
	WaitBelowMoving   = "below_moving"
	WaitBaseRed       = "base_red"
	WaitChecksPending = "checks_pending"
	WaitSettling      = "settling"
	WaitFixPending    = "fix_pending"
	WaitOwnerMerges   = "owner_merges"
	WaitMergeable     = "mergeable_unknown"
	WaitMergedOnForge = "merged_on_forge"
)

// Reconcile compares desired and actual state and returns the plan. It is
// pure and deterministic: no I/O, no clock but in.Now, and the same plan
// whatever the order of in's slices.
func Reconcile(in Input) Plan {
	r := newRun(in)
	r.baseRed()
	r.links()
	for _, c := range r.order {
		r.change(c)
	}
	return r.plan()
}

// run is one Reconcile's indexed view of its input.
type run struct {
	in       Input
	now      time.Time
	p        Policy
	changes  []Change
	order    []*Change // topological: (depth, seq, id)
	rank     map[string]int
	g        *graph
	inBase   map[string]bool
	heads    map[string]string // branch -> tip
	bases    map[string]string // base -> tip
	openPR   map[string]*PR    // branch -> newest open PR
	prByNum  map[int]*PR
	checks   map[string]map[string]Check // head -> name -> check
	required map[string][]string         // base -> required names
	host     map[string]bool
	gates    map[string][]GateResult // head -> results
	attempts map[string]Attempt
	baseBad  map[string]map[string]bool // base -> failing required checks at its tip
	out      []planned
}

type planned struct {
	a    Action
	rank int // the change's topological rank; -1 for base-wide actions
	n    int // emission order
}

func newRun(in Input) *run {
	r := &run{
		in: in, now: in.Now, p: in.State.Policy.withDefaults(),
		rank: map[string]int{}, inBase: map[string]bool{}, heads: map[string]string{},
		bases: map[string]string{}, openPR: map[string]*PR{}, prByNum: map[int]*PR{},
		checks: map[string]map[string]Check{}, required: map[string][]string{},
		host: map[string]bool{}, gates: map[string][]GateResult{}, attempts: map[string]Attempt{},
		baseBad: map[string]map[string]bool{},
	}
	r.changes = append([]Change(nil), in.State.Changes...)
	sort.Slice(r.changes, func(i, j int) bool { return r.changes[i].ID < r.changes[j].ID })
	for _, id := range in.Git.InBase {
		r.inBase[id] = true
	}
	for _, b := range in.Git.Heads {
		r.heads[b.Branch] = b.SHA
	}
	for _, b := range in.Git.Bases {
		r.bases[b.Branch] = b.SHA
	}
	for i := range in.Forge.PRs {
		p := &in.Forge.PRs[i]
		if q := r.prByNum[p.Number]; q == nil {
			r.prByNum[p.Number] = p
		}
		if p.State == PROpen {
			if q := r.openPR[p.Branch]; q == nil || p.Number > q.Number {
				r.openPR[p.Branch] = p
			}
		}
	}
	for _, h := range r.p.HostChecks {
		r.host[h] = true
	}
	for _, c := range in.Forge.Checks {
		if c.HostOwned {
			r.host[c.Name] = true
		}
	}
	for _, c := range in.Forge.Checks {
		m := r.checks[c.Head]
		if m == nil {
			m = map[string]Check{}
			r.checks[c.Head] = m
		}
		if old, ok := m[c.Name]; ok && worse(old.Status, c.Status) {
			continue // two results for one name: the worse wins, whatever the order
		}
		m[c.Name] = c
	}
	for _, rc := range in.Forge.Required {
		if !containsStr(r.required[rc.Base], rc.Name) {
			r.required[rc.Base] = append(r.required[rc.Base], rc.Name)
		}
	}
	for _, g := range in.Git.Gates {
		r.gates[g.Head] = append(r.gates[g.Head], g)
	}
	for _, a := range in.State.Attempts {
		if old, ok := r.attempts[a.Key]; ok && mustJSONless(old, a) {
			continue
		}
		r.attempts[a.Key] = a
	}
	r.g = buildGraph(r.changes, in.State.Edges, r.inBase)
	memo := map[string]int{}
	for i := range r.changes {
		r.order = append(r.order, &r.changes[i])
		r.g.depth(r.changes[i].ID, memo)
	}
	sort.SliceStable(r.order, func(i, j int) bool {
		a, b := r.order[i], r.order[j]
		if memo[a.ID] != memo[b.ID] {
			return memo[a.ID] < memo[b.ID]
		}
		if a.Seq != b.Seq {
			return a.Seq < b.Seq
		}
		return a.ID < b.ID
	})
	for i, c := range r.order {
		r.rank[c.ID] = i
	}
	return r
}

// worse orders check statuses failure > pending > success.
func worse(a, b CheckStatus) bool {
	score := func(s CheckStatus) int {
		switch s {
		case CheckFailure:
			return 2
		case CheckPending:
			return 1
		}
		return 0
	}
	return score(a) >= score(b)
}

// mustJSONless picks between two attempt rows with one key (bad input):
// the one with more attempts, then the later NextAt, wins.
func mustJSONless(keep, other Attempt) bool {
	if keep.N != other.N {
		return keep.N > other.N
	}
	return !other.NextAt.After(keep.NextAt)
}

// --- facts ---

func (r *run) merged(id string) bool {
	c := r.g.byID[id]
	return r.inBase[id] || (c != nil && c.State == Merged)
}

// unmerged returns id's honoured deps that aren't merged, sorted.
func (r *run) unmerged(id string) []string {
	var out []string
	for _, d := range r.g.deps[id] {
		if !r.merged(d) {
			out = append(out, d)
		}
	}
	return out
}

// desired returns the desired base branch and the nearest unmerged dep
// ("" for base). ok is false when the unmerged deps aren't in one chain.
func (r *run) desired(c *Change) (base, nearest string, ok bool) {
	u := r.unmerged(c.ID)
	if len(u) == 0 {
		return c.Base, "", true
	}
	for _, cand := range u {
		all := true
		for _, o := range u {
			if o != cand && !r.g.reaches(cand, o) {
				all = false
				break
			}
		}
		if all {
			return r.g.byID[cand].Branch, cand, true
		}
	}
	return "", "", false
}

// root is the bottom change of c's stack: follow nearest deps down.
func (r *run) root(c *Change) string {
	id := c.ID
	for range len(r.changes) + 1 {
		_, n, ok := r.desired(r.g.byID[id])
		if !ok || n == "" {
			return id
		}
		id = n
	}
	return id
}

func (r *run) onto(c *Change) Onto {
	var heads []string
	for _, d := range r.unmerged(c.ID) {
		heads = append(heads, r.g.byID[d].Head)
	}
	sort.Strings(heads)
	return Onto{Base: c.Base, BaseTip: r.bases[c.Base], Deps: heads}
}

// gate returns the gate result for c's head on onto's base and deps. A
// result on an older base tip still counts (base moving on under a gate
// doesn't re-run it); the current tip wins, then the latest run.
func (r *run) gate(c *Change, on Onto) (GateResult, bool) {
	want := on.depsKey()
	var best GateResult
	found := false
	for _, g := range r.gates[c.Head] {
		g.Onto.Deps = sortedCopy(g.Onto.Deps)
		if g.Onto.depsKey() != want {
			continue
		}
		if !found || betterGate(g, best, on.BaseTip) {
			best, found = g, true
		}
	}
	return best, found
}

func betterGate(a, b GateResult, tip string) bool {
	at, bt := a.Onto.BaseTip == tip, b.Onto.BaseTip == tip
	if at != bt {
		return at
	}
	if !a.RanAt.Equal(b.RanAt) {
		return a.RanAt.After(b.RanAt)
	}
	if a.Onto.BaseTip != b.Onto.BaseTip {
		return a.Onto.BaseTip > b.Onto.BaseTip
	}
	return a.Status < b.Status
}

// requiredAt lists the required, non-host check names for a head on base.
func (r *run) requiredAt(head, base string) []string {
	set := map[string]bool{}
	for _, n := range r.required[base] {
		set[n] = true
	}
	for n, c := range r.checks[head] {
		if c.Required {
			set[n] = true
		}
	}
	var out []string
	for n := range set {
		if !r.host[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// checkState sums up c's required, non-host checks at its head. Checks red
// on base's tip are reported apart: they are base's, not the change's.
type checkState struct {
	failing []string // red here and not on base
	baseRed []string // red here because base is red
	pending bool
}

func (r *run) checkState(c *Change) checkState {
	var s checkState
	for _, n := range r.requiredAt(c.Head, c.Base) {
		ck, ok := r.checks[c.Head][n]
		switch {
		case !ok || ck.Status == CheckPending:
			if r.baseBad[c.Base][n] {
				s.baseRed = append(s.baseRed, n)
			} else {
				s.pending = true
			}
		case ck.Status == CheckFailure:
			if r.baseBad[c.Base][n] {
				s.baseRed = append(s.baseRed, n)
			} else {
				s.failing = append(s.failing, n)
			}
		}
	}
	return s
}

// needsMove reports whether c's open PR is off its desired base.
func (r *run) needsMove(c *Change) bool {
	if c.State.Terminal() || c.State == Queued || r.merged(c.ID) {
		return false
	}
	pr := r.openPR[c.Branch]
	if pr == nil {
		return false
	}
	want, _, ok := r.desired(c)
	return ok && pr.Base != want
}

// belowMoving returns a dependency of c whose PR is off its desired base:
// c's own red is then explained by a Move below it, not by its work.
func (r *run) belowMoving(c *Change) string {
	for _, d := range r.g.closure(c.ID) {
		if r.needsMove(r.g.byID[d]) {
			return d
		}
	}
	return ""
}

// --- emitting ---

func (r *run) emit(c *Change, a Action) {
	rank := -1
	if c != nil {
		rank = r.rank[c.ID]
	}
	r.out = append(r.out, planned{a: a, rank: rank, n: len(r.out)})
}

func (r *run) observedHead(c *Change) string {
	if h := r.heads[c.Branch]; h != "" {
		return h
	}
	return c.Head
}

func (r *run) wait(c *Change, reason string, until time.Time, blocker string) {
	r.emit(c, Action{Kind: ActWait, Change: c.ID, Reason: reason, Until: until, Blocker: blocker})
}

func (r *run) transition(c *Change, to ChangeState, reason string, deadline time.Time) {
	a := Action{Kind: ActTransition, Change: c.ID, To: to, Reason: reason, Deadline: deadline,
		Guard: Guard{Gen: c.Gen, Head: r.observedHead(c)}}
	if pr := r.openPR[c.Branch]; pr != nil && pr.Number != c.PR && !to.Terminal() {
		a.PR = pr.Number
	}
	r.emit(c, a)
}

// escalate emits Escalate once per key.
func (r *run) escalate(c *Change, key, cause string, g Guard) {
	r.escalateOn(c, "", key, cause, g)
}

func (r *run) escalateOn(c *Change, base, key, cause string, g Guard) {
	a := Action{Kind: ActEscalate, Base: base, Key: key, Cause: cause, Guard: g}
	if c != nil {
		a.Change = c.ID
	}
	if r.attempts[a.AttemptKey()].Escalated {
		return
	}
	r.emit(c, a)
}

// forgeWrite reports whether a needs the forge.
func forgeWrite(k ActionKind) bool {
	switch k {
	case ActPublish, ActMove, ActLink, ActMerge, ActClose:
		return true
	}
	return false
}

// try emits a, unless its attempt history says to wait: a failed or
// forge-waiting attempt backs off until NextAt, and a failure repeated
// EscalateAfter times escalates once while retries continue.
func (r *run) try(c *Change, a Action) {
	if r.in.Forge.Degraded && forgeWrite(a.Kind) {
		r.wait(c, WaitForge, r.in.Forge.DegradedUntil, "")
		return
	}
	att, ok := r.attempts[a.AttemptKey()]
	if ok && att.Last.failed() && att.N >= r.p.EscalateAfter {
		r.escalate(c, att.Key, string(a.Kind)+": "+att.LastErr, a.Guard)
	}
	if ok && att.NextAt.After(r.now) && att.Last != OutcomeStale {
		reason := WaitRetry + ":" + string(a.Kind)
		if att.Last == OutcomeOK {
			reason = WaitSettling
			if a.Kind == ActNeedsFix {
				reason = WaitFixPending
			}
		}
		r.wait(c, reason, att.NextAt, "")
		return
	}
	r.emit(c, a)
}

// forgeWaiting reports whether a's last attempt was a forge wait: the clock
// is paused, since the change can't move.
func (r *run) forgeWaiting(a Action) bool {
	return r.attempts[a.AttemptKey()].Last.waiting()
}

// fired reports whether c's deadline has passed and its clock is running.
func (r *run) fired(c *Change) bool {
	return !r.in.Forge.Degraded && !c.Deadline.IsZero() && !r.now.Before(c.Deadline)
}

// step tries a, but if c's deadline fired first (and a isn't waiting on the
// forge) c goes red with cause.
func (r *run) step(c *Change, a Action, cause string) {
	if !r.forgeWaiting(a) && r.fired(c) {
		r.transition(c, Red, cause, time.Time{})
		return
	}
	r.startClock(c)
	r.try(c, a)
}

// startClock gives c the state's default deadline if it has none.
func (r *run) startClock(c *Change) bool {
	if !c.Deadline.IsZero() || r.in.Forge.Degraded {
		return false
	}
	r.transition(c, c.State, c.Reason, r.now.Add(r.deadlineFor(c.State)))
	return true
}

func (r *run) deadlineFor(s ChangeState) time.Duration {
	switch s {
	case Queued:
		return r.p.QueuedDeadline
	case Landed:
		return r.p.LandedDeadline
	case Published:
		return r.p.PublishedDeadline
	case Green:
		return r.p.GreenDeadline
	}
	return 0
}

// --- base-wide ---

// baseRed finds required checks red on each base's tip. Each gets exactly
// one NeedsFix(base, check) and one Escalate per (base tip, check); no
// change is asked to fix it.
func (r *run) baseRed() {
	used := map[string]bool{}
	for _, c := range r.changes {
		if !c.State.Terminal() && !r.inBase[c.ID] {
			used[c.Base] = true
		}
	}
	for _, base := range sortedKeysOf(used) {
		tip := r.bases[base]
		if tip == "" || r.in.Forge.Degraded {
			continue
		}
		for _, n := range r.requiredAt(tip, base) {
			if ck, ok := r.checks[tip][n]; !ok || ck.Status != CheckFailure {
				continue
			}
			if r.baseBad[base] == nil {
				r.baseBad[base] = map[string]bool{}
			}
			r.baseBad[base][n] = true
			g := Guard{BaseTip: tip}
			fix := Action{Kind: ActNeedsFix, Base: base, Cause: CheckCause(n), Guard: g}
			if att, ok := r.attempts[fix.AttemptKey()]; !ok || att.Last != OutcomeOK {
				if !ok || !att.NextAt.After(r.now) {
					r.emit(nil, fix)
				}
			}
			r.escalateOn(nil, base, "base_red/"+base+"/"+tip+"/"+n, CheckCause(n), g)
		}
	}
}

// links emits one Link per stack whose native links don't match the
// desired bases. It looks only at PR bases, never at change states, so a
// red change doesn't change another's links.
func (r *run) links() {
	if !r.in.Forge.NativeStacks || r.in.Forge.Degraded {
		return
	}
	done := map[string]bool{}
	for _, c := range r.order {
		if c.State.Terminal() || r.merged(c.ID) {
			continue
		}
		pr := r.openPR[c.Branch]
		if pr == nil {
			continue
		}
		want, _, ok := r.desired(c)
		if !ok || pr.Base != want {
			continue
		}
		below := 0
		if bp := r.openPR[want]; bp != nil {
			below = bp.Number
		}
		if pr.StackBelow == below {
			continue
		}
		root := r.root(c)
		if done[root] {
			continue
		}
		done[root] = true
		rc := r.g.byID[root]
		r.try(rc, Action{Kind: ActLink, Change: root, Stack: root, Guard: Guard{BaseTip: r.bases[rc.Base]}})
	}
}

func sortedKeysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- per change ---

func (r *run) change(c *Change) {
	pr := r.openPR[c.Branch]
	if c.State.Terminal() {
		if c.State == Superseded && r.inBase[c.ID] {
			r.transition(c, Merged, "in_base", time.Time{})
			return
		}
		if pr != nil && !r.in.Forge.Degraded {
			reason := c.Reason
			if c.State == Merged {
				reason = "merged"
			}
			r.try(c, Action{Kind: ActClose, Change: c.ID, PR: pr.Number, Reason: reason, Guard: Guard{Gen: c.Gen, PRBase: pr.Base}})
		}
		return
	}
	if r.inBase[c.ID] {
		r.transition(c, Merged, "in_base", time.Time{})
		return
	}
	if h := r.heads[c.Branch]; h != "" && h != c.Head {
		// Any new head returns the change to queued.
		a := Action{Kind: ActTransition, Change: c.ID, To: Queued, Reason: "new_head", Head: h,
			Deadline: r.now.Add(r.p.QueuedDeadline), Guard: Guard{Gen: c.Gen, Head: h}}
		r.emit(c, a)
		return
	}
	if c.HoldUntil.After(r.now) {
		r.wait(c, WaitHold, c.HoldUntil, c.ID)
		return
	}
	if c.RetryAt.After(r.now) {
		r.wait(c, WaitRetry, c.RetryAt, c.ID)
		return
	}
	if c.State != Queued && pr == nil && c.PR != 0 {
		if old := r.prByNum[c.PR]; old != nil && old.Branch == c.Branch {
			switch old.State {
			case PRClosed:
				r.transition(c, Superseded, SupersededClosed, time.Time{})
				return
			case PRMerged:
				r.wait(c, WaitMergedOnForge, r.now.Add(r.p.Settle), c.ID)
				return
			}
		}
	}
	if r.in.Forge.Degraded && c.State != Queued {
		r.wait(c, WaitForge, r.in.Forge.DegradedUntil, c.ID)
		return
	}
	want, nearest, ok := r.desired(c)
	if !ok {
		// Two unmerged deps in different stacks: wait until one merges.
		r.wait(c, WaitDepsNotChain, time.Time{}, r.unmerged(c.ID)[0])
		return
	}
	switch c.State {
	case Queued:
		r.queued(c)
	case Landed:
		r.landed(c, pr, want)
	case Published, Green:
		r.published(c, pr, want, nearest)
	case Red:
		r.red(c, pr, want)
	default:
		// An unknown state: treat it as queued so it gets a gate and a clock.
		r.transition(c, Queued, "unknown_state", r.now.Add(r.p.QueuedDeadline))
	}
}

func (r *run) queued(c *Change) {
	on := r.onto(c)
	if g, ok := r.gate(c, on); ok {
		to, cause := Landed, ""
		deadline := r.now.Add(r.p.LandedDeadline)
		switch g.Status {
		case GatePass:
		case GateConflict:
			to, cause, deadline = Red, CauseConflict, time.Time{}
		default:
			to, cause, deadline = Red, CauseGate, time.Time{}
		}
		r.transition(c, to, cause, deadline)
		// The result is for these dependency heads: guard on them.
		last := &r.out[len(r.out)-1].a
		for _, d := range r.unmerged(c.ID) {
			last.Guard.Deps = append(last.Guard.Deps, d+"="+r.g.byID[d].Head)
		}
		return
	}
	r.step(c, Action{Kind: ActGate, Change: c.ID, Onto: &on, Guard: Guard{Head: c.Head, BaseTip: on.BaseTip}}, CauseGate)
}

func (r *run) move(c *Change, pr *PR, want string) Action {
	return Action{Kind: ActMove, Change: c.ID, PR: pr.Number, Base: want, Stack: r.root(c),
		Guard: Guard{Head: c.Head, PRBase: pr.Base}}
}

func (r *run) landed(c *Change, pr *PR, want string) {
	switch {
	case pr == nil:
		r.step(c, Action{Kind: ActPublish, Change: c.ID, Base: want, Guard: Guard{Head: c.Head}}, CausePublish)
	case pr.Base != want:
		// A PR on the wrong base is moved whatever the clock says (I3).
		r.try(c, r.move(c, pr, want))
	default:
		r.transition(c, Published, "", r.now.Add(r.p.PublishedDeadline))
	}
}

// published handles published and green: both have a PR on the desired
// base, and both re-read the checks every tick.
func (r *run) published(c *Change, pr *PR, want, nearest string) {
	if pr == nil {
		r.transition(c, Landed, "pr_missing", r.now.Add(r.p.LandedDeadline))
		return
	}
	if pr.Base != want {
		// I3 before I7: a wrong base is a Move, never a fix.
		r.try(c, r.move(c, pr, want))
		return
	}
	if d := r.belowMoving(c); d != "" {
		r.wait(c, WaitBelowMoving, time.Time{}, d)
		return
	}
	if r.settling(c, pr, want) {
		return
	}
	cs := r.checkState(c)
	switch {
	case len(cs.failing) > 0:
		r.transition(c, Red, CheckCause(cs.failing[0]), time.Time{})
		return
	case len(cs.baseRed) > 0:
		// Base is red: base has its own repair, and this clock pauses.
		r.wait(c, WaitBaseRed, r.now.Add(r.p.Settle), "")
		return
	case cs.pending:
		if c.State == Green {
			r.transition(c, Published, "checks_rerun", r.now.Add(r.p.PublishedDeadline))
			return
		}
		if r.fired(c) {
			r.transition(c, Red, CauseChecksStuck, time.Time{})
			return
		}
		if !r.startClock(c) {
			r.wait(c, WaitChecksPending, c.Deadline, "")
		}
		return
	}
	if c.State == Published {
		deadline := time.Time{}
		if nearest == "" {
			deadline = r.now.Add(r.p.GreenDeadline)
		}
		r.transition(c, Green, "", deadline)
		return
	}
	r.green(c, pr, nearest)
}

// settling waits out the settle time after this head's Publish or Move:
// until then the forge's check results may be the old base's.
func (r *run) settling(c *Change, pr *PR, want string) bool {
	for _, a := range []Action{
		{Kind: ActPublish, Change: c.ID, Base: want, Guard: Guard{Head: c.Head}},
		{Kind: ActMove, PR: pr.Number, Base: want},
	} {
		if att, ok := r.attempts[a.AttemptKey()]; ok && att.Last == OutcomeOK && att.NextAt.After(r.now) {
			r.wait(c, WaitSettling, att.NextAt, "")
			return true
		}
	}
	return false
}

func (r *run) green(c *Change, pr *PR, nearest string) {
	if nearest != "" {
		// The clock runs only once the deps are merged.
		r.wait(c, WaitDepsUnmerged, time.Time{}, nearest)
		return
	}
	if r.startClock(c) {
		return
	}
	if !c.Automerge {
		// The owner merges. At the deadline: one line in the owner's digest
		// and a new deadline. No state is human-only.
		if r.fired(c) {
			r.escalate(c, "digest/"+c.ID+"/"+strconv.FormatInt(c.Deadline.Unix(), 10), "automerge_off", Guard{Gen: c.Gen, Head: c.Head})
			r.transition(c, Green, c.Reason, r.now.Add(r.p.GreenDeadline))
			return
		}
		r.wait(c, WaitOwnerMerges, c.Deadline, "")
		return
	}
	switch pr.Mergeable {
	case MergeConflicting:
		r.transition(c, Red, CauseConflict, time.Time{})
		return
	case MergeUnknown, MergeBlocked:
		if r.fired(c) {
			r.transition(c, Red, CauseMerge, time.Time{})
			return
		}
		r.wait(c, WaitMergeable, minTime(c.Deadline, r.now.Add(r.p.Settle)), "")
		return
	}
	r.step(c, Action{Kind: ActMerge, Change: c.ID, PR: pr.Number, Method: r.p.MergeMethod,
		Guard: Guard{Head: c.Head, PRBase: pr.Base}}, CauseMerge)
}

func minTime(a, b time.Time) time.Time {
	if a.IsZero() || b.Before(a) {
		return b
	}
	return a
}

func (r *run) red(c *Change, pr *PR, want string) {
	if pr != nil && pr.Base != want {
		r.try(c, r.move(c, pr, want))
		return
	}
	if d := r.belowMoving(c); d != "" {
		r.wait(c, WaitBelowMoving, time.Time{}, d)
		return
	}
	cause := c.Reason
	switch {
	case strings.HasPrefix(cause, causeCheckPrefix):
		name := strings.TrimPrefix(cause, causeCheckPrefix)
		if r.baseBad[c.Base][name] {
			r.wait(c, WaitBaseRed, r.now.Add(r.p.Settle), "")
			return
		}
		ck, ok := r.checks[c.Head][name]
		if r.host[name] || !ok || ck.Status != CheckFailure || !containsStr(r.requiredAt(c.Head, c.Base), name) {
			r.back(c, pr)
			return
		}
	case cause == CauseChecksStuck:
		if cs := r.checkState(c); !cs.pending {
			r.back(c, pr)
			return
		}
	case cause == CauseConflict:
		on := r.onto(c)
		rb := Action{Kind: ActRebase, Change: c.ID, Onto: &on, Guard: Guard{Head: c.Head, BaseTip: on.BaseTip}}
		if _, tried := r.attempts[rb.AttemptKey()]; !tried {
			// The host tries a structural rebase before anyone is asked to fix.
			r.try(c, rb)
			return
		}
	}
	fix := r.attempts["needs_fix/"+c.ID]
	if fix.N >= r.p.RepairAttempts {
		r.escalate(c, "gave_up/"+c.ID, cause, Guard{Gen: c.Gen, Head: c.Head})
		r.transition(c, Superseded, SupersededGaveUp, time.Time{})
		return
	}
	r.try(c, Action{Kind: ActNeedsFix, Change: c.ID, Cause: cause, RetryAt: r.now.Add(r.p.FixWait), Guard: Guard{Head: c.Head}})
}

// back returns a red change whose cause has cleared to published (or
// landed, with no PR); the published logic decides from there.
func (r *run) back(c *Change, pr *PR) {
	if pr == nil {
		r.transition(c, Landed, "cleared", r.now.Add(r.p.LandedDeadline))
		return
	}
	r.transition(c, Published, "cleared", r.now.Add(r.p.PublishedDeadline))
}

// --- output ---

// kindOrder: fix bases first (I3 before I7), merge last.
var kindOrder = map[ActionKind]int{
	ActMove: 0, ActLink: 1, ActEscalate: 2, ActNeedsFix: 3, ActTransition: 4, ActPublish: 5,
	ActGate: 6, ActRebase: 7, ActClose: 8, ActMerge: 9, ActWait: 10,
}

func (r *run) plan() Plan {
	out := append([]planned(nil), r.out...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ka, kb := kindOrder[a.a.Kind], kindOrder[b.a.Kind]; ka != kb {
			return ka < kb
		}
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		return a.n < b.n
	})
	p := Plan{Actions: make([]Action, 0, len(out))}
	for _, x := range out {
		p.Actions = append(p.Actions, x.a)
	}
	return p
}

// Collapsible returns the green, dependency-closed groups of changes: each
// change in a group is green and every unmerged dependency it has is in the
// group. It is a view (one integration PR could ship a group); it never
// changes the plan.
func Collapsible(in Input) []Collapse {
	r := newRun(in)
	set := map[string]bool{}
	for _, c := range r.order {
		if c.State == Green && !r.merged(c.ID) {
			set[c.ID] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for _, c := range r.order {
			if !set[c.ID] {
				continue
			}
			for _, d := range r.unmerged(c.ID) {
				if !set[d] {
					delete(set, c.ID)
					changed = true
					break
				}
			}
		}
	}
	// Group by connected component; the root is the first change in
	// topological order.
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		for parent[x] != x {
			x = parent[x]
		}
		return x
	}
	for id := range set {
		parent[id] = id
	}
	for _, c := range r.order {
		if !set[c.ID] {
			continue
		}
		for _, d := range r.unmerged(c.ID) {
			a, b := find(c.ID), find(d)
			if a == b {
				continue
			}
			if r.rank[a] < r.rank[b] {
				parent[b] = a
			} else {
				parent[a] = b
			}
		}
	}
	groups := map[string]*Collapse{}
	var out []Collapse
	var roots []string
	for _, c := range r.order {
		if !set[c.ID] {
			continue
		}
		root := find(c.ID)
		g := groups[root]
		if g == nil {
			g = &Collapse{Root: root}
			groups[root] = g
			roots = append(roots, root)
		}
		g.Changes = append(g.Changes, c.ID)
	}
	for _, root := range roots {
		out = append(out, *groups[root])
	}
	return out
}

// Collapse is one green, dependency-closed group that could ship as one.
type Collapse struct {
	Root    string   `json:"root"`
	Changes []string `json:"changes"`
}
