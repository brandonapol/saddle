package prgraph

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// t0 is the generators' epoch.
var t0 = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

// ciMode is how the model world's CI behaves.
type ciMode int

const (
	ciFixed  ciMode = iota // check facts only change when a test sets them
	ciSilent               // CI never reports: new heads get no results
	ciGreen                // CI reports success on every published head
)

// world is the model world: one in-memory forge, git and store. It
// implements Store, Source and Executor so the real Engine drives it.
type world struct {
	now      time.Time
	st       State
	prs      []PR
	checks   []Check
	required []RequiredCheck
	native   bool
	degraded bool
	bases    map[string]string // base branch -> tip
	heads    map[string]string // change branch -> tip
	inBase   map[string]bool
	gates    []GateResult
	ci       ciMode
	// outcome, when set, decides each executor outcome before any effect.
	outcome func(a Action) Outcome
	// gateStatus, when set, decides a gate's result.
	gateStatus func(change string) GateStatus
	nextPR     int
	applied    []Applied
}

func (w *world) clone() *world {
	b, err := json.Marshal(w.input())
	if err != nil {
		panic(err)
	}
	var in Input
	if err := json.Unmarshal(b, &in); err != nil {
		panic(err)
	}
	c := fromInput(in)
	c.ci, c.outcome, c.gateStatus, c.nextPR = w.ci, w.outcome, w.gateStatus, w.nextPR
	return c
}

func fromInput(in Input) *world {
	w := &world{
		now: in.Now, st: in.State, prs: in.Forge.PRs, checks: in.Forge.Checks,
		required: in.Forge.Required, native: in.Forge.NativeStacks, degraded: in.Forge.Degraded,
		bases: map[string]string{}, heads: map[string]string{}, inBase: map[string]bool{},
		gates: in.Git.Gates, nextPR: 1000,
	}
	for _, b := range in.Git.Bases {
		w.bases[b.Branch] = b.SHA
	}
	for _, h := range in.Git.Heads {
		w.heads[h.Branch] = h.SHA
	}
	for _, id := range in.Git.InBase {
		w.inBase[id] = true
	}
	for _, p := range w.prs {
		if p.Number >= w.nextPR {
			w.nextPR = p.Number + 1
		}
	}
	return w
}

func (w *world) input() Input {
	in := Input{Now: w.now, State: w.st}
	in.State.Changes = append([]Change(nil), w.st.Changes...)
	in.State.Edges = append([]Edge(nil), w.st.Edges...)
	in.State.Attempts = append([]Attempt(nil), w.st.Attempts...)
	in.Forge = ForgeSnapshot{
		PRs: append([]PR(nil), w.prs...), Checks: append([]Check(nil), w.checks...),
		Required: append([]RequiredCheck(nil), w.required...), NativeStacks: w.native,
		Degraded: w.degraded,
	}
	if w.degraded {
		in.Forge.DegradedUntil = w.now.Add(time.Minute)
	}
	for _, b := range sortedKeys(w.bases) {
		in.Git.Bases = append(in.Git.Bases, BranchHead{b, w.bases[b]})
	}
	for _, b := range sortedKeys(w.heads) {
		in.Git.Heads = append(in.Git.Heads, BranchHead{b, w.heads[b]})
	}
	in.Git.InBase = append(in.Git.InBase, sortedKeys(w.inBase)...)
	in.Git.Gates = append([]GateResult(nil), w.gates...)
	return in
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (w *world) change(id string) *Change {
	for i := range w.st.Changes {
		if w.st.Changes[i].ID == id {
			return &w.st.Changes[i]
		}
	}
	return nil
}

func (w *world) openPR(branch string) *PR {
	var best *PR
	for i := range w.prs {
		p := &w.prs[i]
		if p.Branch == branch && p.State == PROpen && (best == nil || p.Number > best.Number) {
			best = p
		}
	}
	return best
}

func (w *world) prByNumber(n int) *PR {
	for i := range w.prs {
		if w.prs[i].Number == n {
			return &w.prs[i]
		}
	}
	return nil
}

// --- Store ---

func (w *world) Load(context.Context) (State, error) { return w.input().State, nil }

func (w *world) Transition(_ context.Context, a Action, now time.Time) (bool, error) {
	c := w.change(a.Change)
	if c == nil || c.Gen != a.Guard.Gen {
		return false, nil
	}
	for _, d := range a.Guard.Deps {
		id, head, _ := strings.Cut(d, "=")
		dc := w.change(id)
		if dc == nil || dc.Head != head || dc.State.Terminal() {
			return false, nil
		}
	}
	if c.State != a.To {
		c.Since = now
	}
	c.State, c.Reason, c.Deadline, c.RetryAt = a.To, a.Reason, a.Deadline, a.RetryAt
	if a.Head != "" {
		c.Head = a.Head
	}
	if a.PR != 0 {
		c.PR = a.PR
	}
	c.Gen++
	return true, nil
}

func (w *world) PutAttempt(_ context.Context, att Attempt) error {
	for i := range w.st.Attempts {
		if w.st.Attempts[i].Key == att.Key {
			w.st.Attempts[i] = att
			return nil
		}
	}
	w.st.Attempts = append(w.st.Attempts, att)
	return nil
}

func (w *world) RecordTick(context.Context, Tick) error { return nil }

// --- Source ---

func (w *world) Snapshot(context.Context, []ChangeRef) (ForgeSnapshot, error) {
	return w.input().Forge, nil
}

func (w *world) GitFacts(context.Context, []ChangeRef) (GitFacts, error) {
	return w.input().Git, nil
}

// --- Executor ---

func (w *world) guardOK(a Action) bool {
	g := a.Guard
	if g.Gen != 0 {
		c := w.change(a.Change)
		if c == nil || c.Gen != g.Gen {
			return false
		}
	}
	if g.Head != "" {
		c := w.change(a.Change)
		if c == nil || w.heads[c.Branch] != g.Head {
			return false
		}
	}
	if g.PRBase != "" {
		p := w.prByNumber(a.PR)
		if p == nil || p.Base != g.PRBase {
			return false
		}
	}
	if g.BaseTip != "" {
		base := a.Base
		if c := w.change(a.Change); c != nil {
			base = c.Base
		}
		if a.Onto != nil {
			base = a.Onto.Base
		}
		if w.bases[base] != g.BaseTip {
			return false
		}
	}
	return true
}

func (w *world) Apply(_ context.Context, a Action) Outcome {
	if !w.guardOK(a) {
		return Outcome{Kind: OutcomeStale}
	}
	if w.outcome != nil {
		if o := w.outcome(a); o.Kind != OutcomeOK {
			return o
		}
	}
	c := w.change(a.Change)
	switch a.Kind {
	case ActGate:
		st := GatePass
		if w.gateStatus != nil {
			st = w.gateStatus(a.Change)
		}
		w.gates = append(w.gates, GateResult{Head: a.Guard.Head, Onto: *a.Onto, Status: st, RanAt: w.now})
	case ActRebase:
		w.heads[c.Branch] = w.heads[c.Branch] + "r"
	case ActPublish:
		if w.openPR(c.Branch) == nil {
			w.prs = append(w.prs, PR{Number: w.nextPR, Branch: c.Branch, Head: w.heads[c.Branch], Base: a.Base, State: PROpen, Mergeable: MergeClean})
			w.nextPR++
			w.ciReport(w.heads[c.Branch])
		}
	case ActMove:
		p := w.prByNumber(a.PR)
		p.Base = a.Base
		w.relink()
		// CI re-runs for the moved PR and every PR stacked on it.
		for _, q := range w.prs {
			if q.State == PROpen {
				w.ciReport(q.Head)
			}
		}
	case ActLink:
		w.relink()
	case ActMerge:
		p := w.prByNumber(a.PR)
		p.State = PRMerged
		for i := range w.st.Changes {
			if w.st.Changes[i].Branch == p.Branch {
				w.inBase[w.st.Changes[i].ID] = true
				w.bases[w.st.Changes[i].Base] += "m"
			}
		}
	case ActClose:
		w.prByNumber(a.PR).State = PRClosed
	}
	return Outcome{Kind: OutcomeOK}
}

func (w *world) relink() {
	if !w.native {
		return
	}
	for i := range w.prs {
		p := &w.prs[i]
		p.StackBelow = 0
		if below := w.openPR(p.Base); below != nil {
			p.StackBelow = below.Number
		}
	}
}

func (w *world) ciReport(head string) {
	if w.ci != ciGreen {
		return
	}
	w.setCheck(Check{Head: head, Name: "ci", Status: CheckSuccess, Required: true})
	w.setCheck(Check{Head: head, Name: "saddle/base", Status: CheckFailure, Required: true, HostOwned: true})
}

func (w *world) setCheck(c Check) {
	for i := range w.checks {
		if w.checks[i].Head == c.Head && w.checks[i].Name == c.Name {
			w.checks[i] = c
			return
		}
	}
	w.checks = append(w.checks, c)
}

// engine drives w with the real Engine.
func (w *world) engine() Engine {
	return Engine{Store: w, Source: w, Executor: recordingExec{w}, Clock: func() time.Time { return w.now }, MaxSteps: 400}
}

type recordingExec struct{ w *world }

func (r recordingExec) Apply(ctx context.Context, a Action) Outcome {
	o := r.w.Apply(ctx, a)
	r.w.applied = append(r.w.applied, Applied{a, o})
	return o
}

// tick runs one Engine tick and fails on an engine error.
func (w *world) tick() []Applied {
	done, err := w.engine().Tick(context.Background())
	if err != nil {
		panic(err)
	}
	return done
}

// --- generators ---

type genOpts struct {
	maxN      int
	clean     bool // every change queued, nothing published, no attempts
	noFailing bool // no red facts: checks pass or pend, gates pass
	baseRed   bool // base's required ci check is red
}

// genWorld builds a random DAG of changes plus random snapshot and git facts.
func genWorld(r *rand.Rand, o genOpts) *world {
	if o.maxN == 0 {
		o.maxN = 12
		if r.Intn(10) == 0 {
			o.maxN = 50
		}
	}
	n := 1 + r.Intn(o.maxN)
	w := &world{
		now: t0, bases: map[string]string{"main": "tip1"}, heads: map[string]string{},
		inBase: map[string]bool{}, nextPR: 1, native: r.Intn(2) == 0,
	}
	w.st.Policy = DefaultPolicy()
	w.required = []RequiredCheck{{Base: "main", Name: "ci"}, {Base: "main", Name: "saddle/base"}}
	w.st.Policy.HostChecks = []string{"saddle/base"}
	states := []ChangeState{Queued, Landed, Published, Green, Red, Merged, Superseded}
	if o.noFailing {
		states = []ChangeState{Queued, Landed, Published, Green}
	}
	for i := range n {
		id := fmt.Sprintf("c%02d", i)
		c := Change{ID: id, Branch: "b/" + id, Base: "main", Head: "h" + id, Seq: int64(i), State: Queued, Automerge: r.Intn(5) != 0, Gen: int64(1 + r.Intn(3))}
		if r.Intn(8) == 0 {
			c.Seq = int64(r.Intn(n)) // ties break on id
		}
		if !o.clean {
			c.State = states[r.Intn(len(states))]
			if c.State == Red {
				c.Reason = []string{CheckCause("ci"), CauseGate, CauseConflict, CauseChecksStuck}[r.Intn(4)]
			}
			if c.State == Superseded {
				c.Reason = []string{SupersededClosed, SupersededKilled, SupersededUnstacked, SupersededGaveUp}[r.Intn(4)]
			}
			c.Deadline = randTime(r)
			if r.Intn(4) == 0 {
				c.RetryAt = randTime(r)
			}
			if r.Intn(8) == 0 {
				c.HoldUntil = randTime(r)
			}
		}
		w.st.Changes = append(w.st.Changes, c)
		w.heads[c.Branch] = c.Head
		if !o.clean && !c.State.Terminal() && r.Intn(10) == 0 {
			w.heads[c.Branch] = c.Head + "x" // a new head
		}
		for j := range i {
			if r.Intn(4) == 0 {
				kind := []EdgeKind{Declared, Inferred, Verified}[r.Intn(3)]
				w.st.Edges = append(w.st.Edges, Edge{Change: id, Dep: fmt.Sprintf("c%02d", j), Kind: kind, Source: "after"})
			}
		}
	}
	if r.Intn(10) == 0 && n > 1 {
		w.st.Edges = append(w.st.Edges, Edge{Change: "c00", Dep: fmt.Sprintf("c%02d", n-1), Kind: Inferred, Source: "issue"}) // a cycle
	}
	if r.Intn(10) == 0 {
		w.st.Edges = append(w.st.Edges, Edge{Change: "c00", Dep: "ghost", Kind: Declared, Source: "after"}) // unknown dep
	}
	if o.clean {
		return w
	}
	for i := range w.st.Changes {
		c := &w.st.Changes[i]
		if c.State == Merged || (r.Intn(12) == 0 && !o.noFailing) {
			w.inBase[c.ID] = true
		}
		if c.State == Queued && r.Intn(3) == 0 {
			continue
		}
		// a PR, on a random base: main or another change's branch
		if r.Intn(5) != 0 {
			base := "main"
			if r.Intn(3) == 0 {
				base = w.st.Changes[r.Intn(n)].Branch
			}
			st := PROpen
			if c.State.Terminal() && r.Intn(2) == 0 {
				st = PRClosed
			}
			if !o.noFailing && r.Intn(15) == 0 {
				st = PRClosed
			}
			mg := []Mergeability{MergeClean, MergeClean, MergeUnknown, MergeConflicting, MergeBlocked}[r.Intn(5)]
			if o.noFailing {
				mg = MergeClean
			}
			w.prs = append(w.prs, PR{Number: w.nextPR, Branch: c.Branch, Head: w.heads[c.Branch], Base: base, State: st, Mergeable: mg, StackBelow: r.Intn(3)})
			c.PR = w.nextPR
			w.nextPR++
		}
		head := w.heads[c.Branch]
		statuses := []CheckStatus{CheckSuccess, CheckSuccess, CheckPending, CheckFailure}
		if o.noFailing {
			statuses = statuses[:3]
		}
		if r.Intn(4) != 0 {
			w.checks = append(w.checks, Check{Head: head, Name: "ci", Status: statuses[r.Intn(len(statuses))], Required: true})
		}
		w.checks = append(w.checks, Check{Head: head, Name: "saddle/base", Status: []CheckStatus{CheckSuccess, CheckFailure}[r.Intn(2)], Required: true, HostOwned: true})
		w.checks = append(w.checks, Check{Head: head, Name: "lint", Status: statuses[r.Intn(len(statuses))]})
		if r.Intn(2) == 0 {
			gs := []GateStatus{GatePass, GatePass, GateFail, GateConflict}[r.Intn(4)]
			if o.noFailing {
				gs = GatePass
			}
			w.gates = append(w.gates, GateResult{Head: head, Onto: oracleOnto(w, c.ID), Status: gs})
		}
		if r.Intn(4) == 0 {
			w.st.Attempts = append(w.st.Attempts, Attempt{Key: "needs_fix/" + c.ID, Kind: "needs_fix", N: r.Intn(5), Last: OutcomeOK, NextAt: randTime(r)})
		}
	}
	baseCI := CheckSuccess
	if o.baseRed {
		baseCI = CheckFailure
	}
	w.checks = append(w.checks, Check{Head: "tip1", Name: "ci", Status: baseCI, Required: true})
	w.checks = append(w.checks, Check{Head: "tip1", Name: "saddle/base", Status: CheckFailure, Required: true, HostOwned: true})
	return w
}

func randTime(r *rand.Rand) time.Time {
	switch r.Intn(4) {
	case 0:
		return time.Time{}
	case 1:
		return t0.Add(-time.Duration(1+r.Intn(300)) * time.Minute)
	default:
		return t0.Add(time.Duration(1+r.Intn(300)) * time.Minute)
	}
}

// --- oracles: independent of reconcile.go ---

// oracleMerged: base contains the work, or the row is merged.
func oracleMerged(w *world, id string) bool {
	c := w.change(id)
	return w.inBase[id] || (c != nil && c.State == Merged)
}

// oracleDeps are the deps Reconcile must honour: known, not superseded
// (unless their work is in base), acyclic. Edges are added in (change seq,
// change id, dep id) order; one that would close a cycle is dropped.
func oracleDeps(w *world, id string) []string {
	g := oracleGraph(w)
	return g[id]
}

func oracleGraph(w *world) map[string][]string {
	edges := append([]Edge(nil), w.st.Edges...)
	seq := map[string]int64{}
	for _, c := range w.st.Changes {
		seq[c.ID] = c.Seq
	}
	sort.Slice(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		if seq[a.Change] != seq[b.Change] {
			return seq[a.Change] < seq[b.Change]
		}
		if a.Change != b.Change {
			return a.Change < b.Change
		}
		return a.Dep < b.Dep
	})
	g := map[string][]string{}
	var reach func(from, to string, seen map[string]bool) bool
	reach = func(from, to string, seen map[string]bool) bool {
		if from == to {
			return true
		}
		if seen[from] {
			return false
		}
		seen[from] = true
		for _, d := range g[from] {
			if reach(d, to, seen) {
				return true
			}
		}
		return false
	}
	for _, e := range edges {
		d := w.change(e.Dep)
		if d == nil || e.Dep == e.Change || w.change(e.Change) == nil {
			continue
		}
		if d.State == Superseded && !w.inBase[d.ID] {
			continue
		}
		if contains(g[e.Change], e.Dep) || reach(e.Dep, e.Change, map[string]bool{}) {
			continue
		}
		g[e.Change] = append(g[e.Change], e.Dep)
	}
	for k := range g {
		sort.Strings(g[k])
	}
	return g
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func oracleUnmerged(w *world, id string) []string {
	var out []string
	for _, d := range oracleDeps(w, id) {
		if !oracleMerged(w, d) {
			out = append(out, d)
		}
	}
	return out
}

// oraclePath: from depends on to through honoured edges.
func oraclePath(w *world, from, to string) bool {
	g := oracleGraph(w)
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(x string) bool {
		if x == to {
			return true
		}
		if seen[x] {
			return false
		}
		seen[x] = true
		for _, d := range g[x] {
			if walk(d) {
				return true
			}
		}
		return false
	}
	return walk(from)
}

// oracleDesiredBase: the nearest unmerged dep's branch, else base; ok is
// false when the unmerged deps aren't in one chain.
func oracleDesiredBase(w *world, id string) (string, bool) {
	c := w.change(id)
	u := oracleUnmerged(w, id)
	if len(u) == 0 {
		return c.Base, true
	}
	for _, cand := range u {
		all := true
		for _, o := range u {
			if o != cand && !oraclePath(w, cand, o) {
				all = false
				break
			}
		}
		if all {
			return w.change(cand).Branch, true
		}
	}
	return "", false
}

func oracleOnto(w *world, id string) Onto {
	c := w.change(id)
	var heads []string
	for _, d := range oracleUnmerged(w, id) {
		heads = append(heads, w.change(d).Head)
	}
	sort.Strings(heads)
	return Onto{Base: c.Base, BaseTip: w.bases[c.Base], Deps: heads}
}

// shuffled returns in with every slice shuffled.
func shuffled(r *rand.Rand, in Input) Input {
	b, _ := json.Marshal(in)
	var out Input
	_ = json.Unmarshal(b, &out)
	sh := func(n int, swap func(i, j int)) { r.Shuffle(n, swap) }
	s := &out.State
	sh(len(s.Changes), func(i, j int) { s.Changes[i], s.Changes[j] = s.Changes[j], s.Changes[i] })
	sh(len(s.Edges), func(i, j int) { s.Edges[i], s.Edges[j] = s.Edges[j], s.Edges[i] })
	sh(len(s.Attempts), func(i, j int) { s.Attempts[i], s.Attempts[j] = s.Attempts[j], s.Attempts[i] })
	f := &out.Forge
	sh(len(f.PRs), func(i, j int) { f.PRs[i], f.PRs[j] = f.PRs[j], f.PRs[i] })
	sh(len(f.Checks), func(i, j int) { f.Checks[i], f.Checks[j] = f.Checks[j], f.Checks[i] })
	sh(len(f.Required), func(i, j int) { f.Required[i], f.Required[j] = f.Required[j], f.Required[i] })
	g := &out.Git
	sh(len(g.Bases), func(i, j int) { g.Bases[i], g.Bases[j] = g.Bases[j], g.Bases[i] })
	sh(len(g.Heads), func(i, j int) { g.Heads[i], g.Heads[j] = g.Heads[j], g.Heads[i] })
	sh(len(g.InBase), func(i, j int) { g.InBase[i], g.InBase[j] = g.InBase[j], g.InBase[i] })
	sh(len(g.Gates), func(i, j int) { g.Gates[i], g.Gates[j] = g.Gates[j], g.Gates[i] })
	for i := range g.Gates {
		d := g.Gates[i].Onto.Deps
		sh(len(d), func(a, b int) { d[a], d[b] = d[b], d[a] })
	}
	return out
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// depth is the longest dependency chain in w's honoured graph.
func depth(w *world) int {
	g := oracleGraph(w)
	memo := map[string]int{}
	var d func(string) int
	d = func(x string) int {
		if v, ok := memo[x]; ok {
			return v
		}
		best := 0
		for _, y := range g[x] {
			if v := d(y) + 1; v > best {
				best = v
			}
		}
		memo[x] = best
		return best
	}
	m := 0
	for _, c := range w.st.Changes {
		if v := d(c.ID); v > m {
			m = v
		}
	}
	return m
}
