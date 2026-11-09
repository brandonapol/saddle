package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/automerge"
	"github.com/brandonapol/saddle/internal/forge"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/prgraph"
	"github.com/brandonapol/saddle/internal/store"
)

// ShadowInterval is how often the shadow reconciles: the sentinel's tick.
const ShadowInterval = 2 * time.Minute

// shadowTickKeep is how long recorded shadow ticks are kept (ADR 0009 §1).
const shadowTickKeep = 14 * 24 * time.Hour

// shadowEventLookback is how many recent events seed the shadow's clocks
// and edge kinds when it starts.
const shadowEventLookback = 5000

// shadowEdge reads one depends_on event as edges: a comma list at spawn
// (declared after edges), or one learned edge with how saddle learned it.
func shadowEdges(task, data string) []prgraph.Edge {
	data = strings.TrimSpace(data)
	if dep, why, ok := strings.Cut(data, " ("); ok {
		why = strings.TrimSuffix(why, ")")
		e := prgraph.Edge{Change: task, Dep: strings.TrimSpace(dep), Kind: prgraph.Declared, Source: "after"}
		switch {
		case strings.HasPrefix(why, "inferred"):
			e.Kind, e.Source = prgraph.Inferred, "issue"
		case strings.Contains(why, "saddle after"):
		default:
			e.Source = "hand_retarget"
		}
		return []prgraph.Edge{e}
	}
	var out []prgraph.Edge
	for _, d := range strings.Split(data, ",") {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, prgraph.Edge{Change: task, Dep: d, Kind: prgraph.Declared, Source: "after"})
		}
	}
	return out
}

// shadowSinceKinds are the events that start a legacy state, for the
// change's clock.
var shadowSinceKinds = map[string]bool{
	"done": true, "landed": true, "pr_layout": true, "ci_red": true, "restack": true, "train_test_failed": true, "train_conflict": true,
}

// shadowHistory is what the shadow learns from the event log as it goes:
// when each task's legacy state began, how each after edge was learned,
// and which tasks had a PR.
type shadowHistory struct {
	since map[string]time.Time
	edges map[string]prgraph.Edge // "task>dep"
	prOf  map[string]int
}

func newShadowHistory() *shadowHistory {
	return &shadowHistory{since: map[string]time.Time{}, edges: map[string]prgraph.Edge{}, prOf: map[string]int{}}
}

func (h *shadowHistory) add(ev store.Event) {
	if shadowSinceKinds[ev.Kind] && ev.Task != "" {
		h.since[ev.Task] = ev.TS
	}
	if ev.Kind == EventDependsOn {
		for _, e := range shadowEdges(ev.Task, ev.Data) {
			h.edges[e.Change+">"+e.Dep] = e
		}
	}
}

// shadowStores reads today's stores. after is each task's deps/<task>.
func (a *App) shadowStores(now time.Time, h *shadowHistory) (ShadowStores, error) {
	s := ShadowStores{Now: now, Base: a.Cfg.Base, Since: h.since, Published: a.publishedPRs()}
	var err error
	if s.Tasks, err = a.Store.Tasks(); err != nil {
		return s, err
	}
	if s.Train, err = a.Store.Train(); err != nil {
		return s, err
	}
	for _, e := range s.Train {
		for _, d := range a.TaskAfter(e.Task) {
			edge, ok := h.edges[e.Task+">"+d]
			if !ok {
				edge = prgraph.Edge{Change: e.Task, Dep: d, Kind: prgraph.Declared, Source: "after"}
			}
			s.Edges = append(s.Edges, edge)
		}
	}
	if s.Stacks, err = a.CustomStacks(); err != nil {
		return s, err
	}
	if s.CIRed, err = a.CIRed(); err != nil {
		return s, err
	}
	if st, err := automerge.Load(a.automergePath()); err == nil {
		s.Automerge = a.Cfg.Train.AutoMerge
		if st.Enabled != nil {
			s.Automerge = *st.Enabled
		}
		s.AutomergeHolds = st.Holds
	}
	s.Policy = prgraph.Policy{RepairAttempts: a.Cfg.CI.RepairAttempts}
	return s, nil
}

// shadowSource is the shadow's read-only view of the forge and git. It
// reads the forge in one batched call per tick: gh runs under the shared
// rate-limit governor (#764), and a limit or transient failure gives a
// degraded snapshot, never an error.
type shadowSource struct {
	a     *App
	forge forge.Forge
}

var _ prgraph.Source = shadowSource{}

func (s shadowSource) Snapshot(ctx context.Context, _ []prgraph.ChangeRef) (prgraph.ForgeSnapshot, error) {
	prs, err := s.forge.ListPRs(ctx, forge.PRQuery{State: "open", Limit: 200},
		forge.PRNumber|forge.PRState|forge.PRBase|forge.PRHeadBranch|forge.PRHead|forge.PRMergeable|forge.PRChecks)
	if err != nil {
		until, ok := forge.RateLimitUntil(err)
		if !ok {
			until = time.Now().Add(ShadowInterval)
		}
		return prgraph.ForgeSnapshot{Degraded: true, DegradedUntil: until}, nil
	}
	return forgeSnapshot(prs), nil
}

// forgeSnapshot converts the forge's open PRs and their check rollups.
func forgeSnapshot(prs []forge.PR) prgraph.ForgeSnapshot {
	var snap prgraph.ForgeSnapshot
	for _, p := range prs {
		pr := prgraph.PR{Number: p.Number, Branch: p.Head, Head: p.HeadSHA, Base: p.Base, State: prgraph.PROpen}
		switch strings.ToUpper(p.State) {
		case forge.StateMerged:
			pr.State = prgraph.PRMerged
		case forge.StateClosed:
			pr.State = prgraph.PRClosed
		}
		switch p.Mergeable {
		case forge.Mergeable:
			pr.Mergeable = prgraph.MergeClean
		case forge.Conflicting:
			pr.Mergeable = prgraph.MergeConflicting
		}
		snap.PRs = append(snap.PRs, pr)
		for _, c := range p.Checks {
			name := c.Context
			if c.Kind != forge.KindStatus {
				name = c.Name
				if c.Workflow != "" {
					name = c.Workflow + " / " + c.Name
				}
			}
			if name == "" {
				continue
			}
			snap.Checks = append(snap.Checks, prgraph.Check{Head: p.HeadSHA, Name: name, Status: checkStatus(c),
				Required: true})
		}
	}
	return snap
}

func checkStatus(c forge.CheckRun) prgraph.CheckStatus {
	v := strings.ToUpper(c.Conclusion)
	if c.Kind == forge.KindStatus || (c.Conclusion == "" && c.State != "") {
		v = strings.ToUpper(c.State)
	} else if c.Status != "" && strings.ToUpper(c.Status) != "COMPLETED" {
		return prgraph.CheckPending
	}
	switch v {
	case "SUCCESS", "SKIPPED", "NEUTRAL":
		return prgraph.CheckSuccess
	case "FAILURE", "ERROR", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE":
		return prgraph.CheckFailure
	}
	return prgraph.CheckPending
}

func (s shadowSource) GitFacts(_ context.Context, changes []prgraph.ChangeRef) (prgraph.GitFacts, error) {
	a := s.a
	var g prgraph.GitFacts
	if tip, err := gitx.Run(a.Root, "rev-parse", "--verify", "-q", a.Cfg.Base+"^{commit}"); err == nil && tip != "" {
		g.Bases = append(g.Bases, prgraph.BranchHead{Branch: a.Cfg.Base, SHA: tip})
	}
	refs := make([]string, 0, len(changes))
	for _, c := range changes {
		if c.Branch != "" {
			refs = append(refs, "refs/heads/"+c.Branch)
		}
	}
	if len(refs) > 0 {
		// One git process for every branch tip.
		args := append([]string{"for-each-ref", "--format=%(refname:lstrip=2) %(objectname)"}, refs...)
		if out, err := gitx.Run(a.Root, args...); err == nil {
			for _, line := range strings.Split(out, "\n") {
				if b, sha, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
					g.Heads = append(g.Heads, prgraph.BranchHead{Branch: b, SHA: sha})
				}
			}
		}
	}
	// The work is in base when a commit there carries the change's
	// Saddle-Task trailer (squash merges keep it).
	want := map[string]bool{}
	for _, c := range changes {
		want[c.ID] = true
	}
	if out, err := gitx.Run(a.Root, "log", "-n", "3000", "--format=%(trailers:key=Saddle-Task,valueonly,separator=%x2C)", a.Cfg.Base); err == nil {
		in := map[string]bool{}
		for _, line := range strings.Split(out, "\n") {
			for _, id := range strings.Split(line, ",") {
				if id = strings.TrimSpace(id); want[id] && !in[id] {
					in[id] = true
					g.InBase = append(g.InBase, id)
				}
			}
		}
	}
	return g, nil
}

// ShadowReconciler is the supervised shadow watcher: every tick it builds
// prgraph's Input from today's stores, settles a plan, and logs where it
// and saddle's own actions disagree. It never acts.
type ShadowReconciler struct {
	App      *App
	Source   prgraph.Source
	Interval time.Duration
	Clock    func() time.Time
	// Ticks, when set, records each tick whose input changed, for replay.
	Ticks *prgraph.SQLStore

	diff     *shadowDiff
	hist     *shadowHistory
	cursor   int64
	lastHash string
	started  bool
}

// NewShadowReconciler returns the shadow watcher over the repo's forge.
// It records ticks in prgraph's tables in state.db when it can open them.
func (a *App) NewShadowReconciler() *ShadowReconciler {
	r := &ShadowReconciler{App: a, Source: shadowSource{a: a, forge: a.Forge()}, Interval: ShadowInterval}
	if db, err := sql.Open("sqlite", "file:"+a.stateDir("state.db")+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)"); err == nil {
		if st, err := prgraph.OpenSQLStore(context.Background(), db, prgraph.Policy{}); err == nil {
			r.Ticks = st
		} else {
			_ = db.Close()
		}
	}
	return r
}

// Run ticks until ctx ends. A failed tick is logged and the next one runs.
func (r *ShadowReconciler) Run(ctx context.Context) {
	if r.Ticks != nil {
		defer r.Ticks.DB.Close()
	}
	every := r.Interval
	if every <= 0 {
		every = ShadowInterval
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := r.Tick(ctx); err != nil && ctx.Err() == nil {
			r.App.Store.Event("", EventReconcileShadow, "error: "+err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (r *ShadowReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

// start seeds the history from recent events and sets the event cursor, so
// legacy actions from before the shadow started aren't judged.
func (r *ShadowReconciler) start() error {
	r.diff = newShadowDiff(ShadowWindow)
	r.hist = newShadowHistory()
	evs, err := r.App.Store.Events(shadowEventLookback)
	if err != nil {
		return err
	}
	for _, ev := range evs {
		r.hist.add(ev)
		r.cursor = max(r.cursor, ev.ID)
	}
	r.started = true
	return nil
}

// Tick reconciles once in shadow and logs each divergence as a
// reconcile_shadow event. It returns them.
func (r *ShadowReconciler) Tick(ctx context.Context) ([]ShadowDivergence, error) {
	if !r.started {
		if err := r.start(); err != nil {
			return nil, err
		}
	}
	now := r.now()
	evs, err := r.App.Store.EventsSince(r.cursor)
	if err != nil {
		return nil, err
	}
	var legacy []LegacyAction
	for _, ev := range evs {
		r.cursor = max(r.cursor, ev.ID)
		if ev.Kind == EventReconcileShadow {
			continue
		}
		if la, ok := legacyAction(ev, r.hist.prOf); ok {
			legacy = append(legacy, la)
		}
		r.hist.add(ev)
	}
	raw, plan, err := r.App.shadowPlan(ctx, now, r.hist, r.Source)
	if err != nil {
		return nil, err
	}
	for _, c := range raw.State.Changes {
		if c.PR != 0 {
			r.hist.prOf[c.ID] = c.PR
		}
	}
	r.record(ctx, raw)
	out := r.diff.Observe(now, plan, legacy)
	for _, d := range out {
		r.App.Store.Event(d.Change, EventReconcileShadow, d.Line())
	}
	return out, nil
}

// record stores the adapter's raw input, with the plan Reconcile gives it,
// when that input changed (the clock aside), and prunes ticks past their
// keep. prgraph.Replay reproduces the recorded plan from it byte for byte.
func (r *ShadowReconciler) record(ctx context.Context, in prgraph.Input) {
	if r.Ticks == nil {
		return
	}
	still := in
	still.Now = time.Time{}
	still.State.Changes = append([]prgraph.Change(nil), in.State.Changes...)
	for i := range still.State.Changes {
		if c := &still.State.Changes[i]; c.Since.Equal(in.Now) {
			c.Since = time.Time{}
		}
	}
	b, err := json.Marshal(still)
	if err != nil {
		return
	}
	sum := sha256.Sum256(b)
	h := hex.EncodeToString(sum[:])
	if h == r.lastHash {
		return
	}
	t, err := prgraph.NewTick(in, prgraph.Reconcile(in))
	if err != nil {
		return
	}
	if err := r.Ticks.RecordTick(ctx, t); err == nil {
		r.lastHash = h
		_, _ = r.Ticks.PruneTicks(ctx, in.Now.Add(-shadowTickKeep))
	}
}

// shadowPlan builds the shadow's input from the stores and source and
// settles its plan. It returns the adapter's raw input, before settling.
func (a *App) shadowPlan(ctx context.Context, now time.Time, h *shadowHistory, src prgraph.Source) (prgraph.Input, prgraph.Plan, error) {
	s, err := a.shadowStores(now, h)
	if err != nil {
		return prgraph.Input{}, prgraph.Plan{}, err
	}
	pre := ShadowInput(s, prgraph.ForgeSnapshot{}, prgraph.GitFacts{})
	refs := make([]prgraph.ChangeRef, 0, len(pre.State.Changes))
	for _, c := range pre.State.Changes {
		refs = append(refs, prgraph.ChangeRef{ID: c.ID, Branch: c.Branch, Base: c.Base, PR: c.PR})
	}
	snap, err := src.Snapshot(ctx, refs)
	if err != nil {
		return prgraph.Input{}, prgraph.Plan{}, fmt.Errorf("shadow: forge: %w", err)
	}
	gf, err := src.GitFacts(ctx, refs)
	if err != nil {
		return prgraph.Input{}, prgraph.Plan{}, fmt.Errorf("shadow: git: %w", err)
	}
	raw := ShadowInput(s, snap, gf)
	_, plan := SettlePlan(raw)
	return raw, plan, nil
}

// ShadowPlan is `saddle reconcile --plan`: prgraph's settled plan for the
// stack as it is now. It reads the forge once and changes nothing.
func (a *App) ShadowPlan(ctx context.Context) (prgraph.Input, prgraph.Plan, error) {
	h := newShadowHistory()
	evs, err := a.Store.Events(shadowEventLookback)
	if err != nil {
		return prgraph.Input{}, prgraph.Plan{}, err
	}
	for _, ev := range evs {
		h.add(ev)
	}
	return a.shadowPlan(ctx, time.Now(), h, shadowSource{a: a, forge: a.Forge()})
}

// RunShadowReconciler is the supervised watcher's entry point (#789).
func (a *App) RunShadowReconciler(ctx context.Context) { a.NewShadowReconciler().Run(ctx) }
