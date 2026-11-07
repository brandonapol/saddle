package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/prgraph"
	"github.com/brandonapol/saddle/internal/store"
)

// `saddle reconcile --replay <state.db>` (#830, ADR 0009 §6.3). Old
// databases have no recorded forge snapshots, so the replay rebuilds
// today's stores from the events table as time passes, feeds them through
// the same adapter and diff the live shadow uses, and reports where prgraph
// and saddle's own code part ways. It judges outcomes, not bytes. Where the
// database has recorded prgraph ticks, each one is also replayed and must
// give its recorded plan.
//
// What the events don't carry is assumed, and the report says so:
//   - a change's head is the last sha saddle logged for it (landed, layout,
//     restack); every commit it pushes is the same work;
//   - CI on a pushed head is pending for ShadowCIRun, then green unless a
//     ci_red event names a failing check;
//   - an open PR is mergeable;
//   - base's tip moves on each merge.

// ShadowCIRun is how long the replay assumes CI runs after a push.
const ShadowCIRun = 30 * time.Minute

// ReplayOptions bound a replay.
type ReplayOptions struct {
	// From and Until bound the window divergences are judged in.
	From, Until time.Time
	// Lookback is how much history before From builds the starting state
	// (default 48h).
	Lookback time.Duration
	// Tick is the simulated sentinel interval (default ShadowInterval).
	Tick time.Duration
	// Window is the shadow's match window (default ShadowWindow).
	Window time.Duration
	// Base is the base branch (default main).
	Base   string
	Policy prgraph.Policy
}

// ReplayOutcomes are ADR 0009 §6.3's outcome counts for the window.
type ReplayOutcomes struct {
	// LegacyRepairs are repairs saddle spawned (ci_repair_spawned), and
	// LegacyHostRepairs those whose layer was red only on a host-owned
	// status (saddle/base).
	LegacyRepairs     int `json:"legacy_repairs"`
	LegacyHostRepairs int `json:"legacy_host_repairs"`
	// PrgraphFixes are the changes prgraph would have asked to fix.
	PrgraphFixes int `json:"prgraph_fixes"`
	// AtRiskSpells are stack_at_risk flags saddle raised; prgraph computes
	// holds and has none.
	AtRiskSpells int `json:"at_risk_spells"`
	// Changes are the changes the window saw; Merged how many merged.
	Changes int `json:"changes"`
	Merged  int `json:"merged"`
}

// ReplayReport is what a replay found.
type ReplayReport struct {
	From        time.Time          `json:"from"`
	Until       time.Time          `json:"until"`
	Events      int                `json:"events"`
	Reconciles  int                `json:"reconciles"`
	Divergences []ShadowDivergence `json:"divergences"`
	// Unknown counts legacy actions on changes the replay never saw done.
	Unknown  int            `json:"unknown"`
	Outcomes ReplayOutcomes `json:"outcomes"`
	// RecordedTicks are prgraph ticks the database recorded in the window,
	// and RecordedDiffer those whose replay gave another plan.
	RecordedTicks  int      `json:"recorded_ticks"`
	RecordedDiffer int      `json:"recorded_differ"`
	Assumptions    []string `json:"assumptions"`
}

var replayAssumptions = []string{
	"a change's head is the last sha saddle logged for it",
	fmt.Sprintf("CI on a pushed head is pending for %s, then green unless a ci_red event names a failing check", ShadowCIRun),
	"an open PR is mergeable",
	"gate facts and dependency edges are only those the events record",
}

// ReplayShadow replays the database at path. It reads a consistent copy
// made in the temp dir (the scratch root under saddle) and never writes to
// path.
func ReplayShadow(ctx context.Context, path string, o ReplayOptions) (ReplayReport, error) {
	if o.Until.IsZero() || !o.From.Before(o.Until) {
		return ReplayReport{}, errors.New("replay: from must come before until")
	}
	if o.Lookback <= 0 {
		o.Lookback = 48 * time.Hour
	}
	if o.Tick <= 0 {
		o.Tick = ShadowInterval
	}
	if o.Window <= 0 {
		o.Window = ShadowWindow
	}
	if o.Base == "" {
		o.Base = "main"
	}
	if _, err := os.Stat(path); err != nil {
		return ReplayReport{}, err
	}
	dir, err := os.MkdirTemp("", "saddle-replay-")
	if err != nil {
		return ReplayReport{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	cp := filepath.Join(dir, "state.db")
	src, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		return ReplayReport{}, err
	}
	_, err = src.ExecContext(ctx, `VACUUM INTO ?`, cp)
	_ = src.Close()
	if err != nil {
		return ReplayReport{}, fmt.Errorf("replay: copy %s: %w", path, err)
	}
	db, err := sql.Open("sqlite", "file:"+cp+"?mode=ro")
	if err != nil {
		return ReplayReport{}, err
	}
	defer db.Close()
	w, err := loadReplayWorld(ctx, db, o)
	if err != nil {
		return ReplayReport{}, err
	}
	rep := w.run(o)
	rep.RecordedTicks, rep.RecordedDiffer = replayRecorded(ctx, db, o)
	return rep, nil
}

// replayRecorded replays the prgraph ticks recorded in the window, if the
// database has any.
func replayRecorded(ctx context.Context, db *sql.DB, o ReplayOptions) (n, differ int) {
	st := &prgraph.SQLStore{DB: db}
	ts, err := st.Ticks(ctx, o.From, o.Until)
	if err != nil {
		return 0, 0 // an old database: no prgraph tables
	}
	for _, t := range ts {
		n++
		if _, same, err := prgraph.Replay(t); err != nil || !same {
			differ++
		}
	}
	return n, differ
}

// rChange is one change as the replay knows it from events.
type rChange struct {
	task     store.Task
	seq      int64
	train    string
	head     string
	pr       int
	prOpen   bool
	prBase   string
	pushedAt time.Time
	checks   map[string]prgraph.CheckStatus
	repairs  int
	inBase   bool
	// stackBelow is the PR below this one in its native stack.
	stackBelow int
}

type replayWorld struct {
	events   []store.Event
	tasks    map[string]store.Task
	changes  map[string]*rChange
	seq      int64
	merges   int
	edges    map[string]prgraph.Edge
	hist     *shadowHistory
	required map[string]bool
	policy   prgraph.Policy
	base     string
	// native: saddle linked native stacks (gh-stack), so links compare.
	native bool
}

func loadReplayWorld(ctx context.Context, db *sql.DB, o ReplayOptions) (*replayWorld, error) {
	w := &replayWorld{tasks: map[string]store.Task{}, changes: map[string]*rChange{}, edges: map[string]prgraph.Edge{},
		hist: newShadowHistory(), required: map[string]bool{}, policy: o.Policy, base: o.Base}
	if w.policy.RepairAttempts <= 0 {
		w.policy.RepairAttempts = 2 // saddle's [ci] repair_attempts default
	}
	rows, err := db.QueryContext(ctx, `SELECT id, title, role, branch, status, pr FROM tasks`)
	if err != nil {
		return nil, fmt.Errorf("replay: tasks: %w", err)
	}
	for rows.Next() {
		var t store.Task
		if err := rows.Scan(&t.ID, &t.Title, &t.Role, &t.Branch, &t.Status, &t.PR); err != nil {
			_ = rows.Close()
			return nil, err
		}
		w.tasks[t.ID] = t
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	rows, err = db.QueryContext(ctx, `SELECT id, ts, task, kind, data FROM events WHERE ts >= ? AND ts < ? ORDER BY id`,
		o.From.Add(-o.Lookback).Unix(), o.Until.Unix())
	if err != nil {
		return nil, fmt.Errorf("replay: events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e store.Event
		var ts int64
		if err := rows.Scan(&e.ID, &ts, &e.Task, &e.Kind, &e.Data); err != nil {
			return nil, err
		}
		e.TS = time.Unix(ts, 0).UTC()
		w.events = append(w.events, e)
		if e.Kind == EventCIRedKind || e.Kind == "ci_recovered" {
			for _, n := range ciNames(e) {
				if n != baseStatus {
					w.required[n] = true
				}
			}
		}
	}
	return w, rows.Err()
}

// EventCIRedKind is the ci-red watcher's event kind.
const EventCIRedKind = "ci_red"

// ciNames are the checks a ci_red ("<sha> a, b") or ci_recovered ("a")
// event names.
func ciNames(e store.Event) []string {
	data := e.Data
	if e.Kind == EventCIRedKind {
		_, data, _ = strings.Cut(data, " ")
	}
	var out []string
	for _, n := range strings.Split(data, ", ") {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

var shaArrowRe = regexp.MustCompile(`(\S+)\s+→\s+([0-9a-f]{7,40})\b`)

func (w *replayWorld) change(id string) *rChange { return w.changes[id] }

// push records a new head pushed for c: CI starts over on it.
func (c *rChange) push(head string, at time.Time) {
	if head != "" {
		c.head = head
	}
	c.pushedAt = at
	c.checks = map[string]prgraph.CheckStatus{}
}

// pre applies what saddle knew when it took ev's action, and returns the
// legacy action, if ev is one.
func (w *replayWorld) pre(ev store.Event) (LegacyAction, bool) {
	c := w.change(ev.Task)
	if c != nil {
		switch ev.Kind {
		case "automerge_merged":
			// What saddle saw: checks green on an open PR.
			for n := range w.required {
				c.checks[n] = prgraph.CheckSuccess
			}
			c.pushedAt = time.Time{}
			w.openPR(c, ev.TS)
		case "done_gate_failed", "train_test_failed", "train_conflict":
			// The gate's result is a fact prgraph would see too.
			w.post(ev)
		}
	}
	prOf := map[string]int{}
	if c != nil && c.prOpen {
		prOf[c.task.ID] = c.pr
	}
	switch ev.Kind {
	case "restack":
		if c != nil && c.train == store.TrainOK && !c.prOpen && !c.inBase {
			// A restack opens the PRs of landed layers that have none.
			return LegacyAction{At: ev.TS, Detail: ev.Kind + " " + ev.Data,
				Action: prgraph.Action{Kind: prgraph.ActPublish, Change: ev.Task}}, true
		}
	case "automerge_merged":
		la, ok := legacyAction(ev, prOf)
		if _, how, found := strings.Cut(ev.Data, " by "); found {
			la.Method, _, _ = strings.Cut(how, ":")
		}
		return la, ok
	case "restack_retarget", EventPRBase:
		m := arrowRe.FindStringSubmatch(ev.Data)
		if m == nil || c == nil || c.prBase == "" || m[3] == c.prBase {
			return LegacyAction{}, false
		}
		la, _ := legacyAction(store.Event{TS: ev.TS, Task: ev.Task, Kind: ev.Kind, Data: m[1] + " " + c.prBase + " → " + m[3]}, prOf)
		la.Detail = ev.Kind + " " + ev.Data
		return la, true
	case "done_gate_failed":
		return LegacyAction{At: ev.TS, Detail: ev.Kind + " " + ev.Data,
			Action: prgraph.Action{Kind: prgraph.ActNeedsFix, Change: ev.Task, Cause: prgraph.CauseGate}}, true
	}
	return legacyAction(ev, prOf)
}

// openPR records that c's PR is open: saddle opened it, or an automerge
// event shows it. Its base is base until an event says otherwise.
func (w *replayWorld) openPR(c *rChange, at time.Time) {
	if c.prOpen || c.inBase {
		return
	}
	c.prOpen = true
	if c.prBase == "" {
		c.prBase = w.base
	}
	c.push("", at)
}

// post applies ev's effect on the world.
func (w *replayWorld) post(ev store.Event) {
	w.hist.add(ev)
	if ev.Kind == EventDependsOn {
		for _, e := range shadowEdges(ev.Task, ev.Data) {
			w.edges[e.Change+">"+e.Dep] = e
		}
		return
	}
	c := w.change(ev.Task)
	if ev.Kind == "done_called" && c == nil {
		t, ok := w.tasks[ev.Task]
		if !ok || t.Branch == "" || t.Role == store.RoleOrchestrator {
			return
		}
		w.seq++
		c = &rChange{task: t, seq: w.seq, pr: prNumberOf(t.PR), checks: map[string]prgraph.CheckStatus{}}
		if c.pr == 0 {
			c.pr = 100000 + int(w.seq)
		}
		c.task.Status = store.Done
		w.changes[ev.Task] = c
	}
	if c == nil {
		return
	}
	switch ev.Kind {
	case "done_called":
		c.train = store.Queued
		c.push(c.task.ID+"@"+strconv.FormatInt(ev.ID, 10), ev.TS)
	case "done":
		if c.train != store.TrainOK {
			c.train = store.Queued
		}
	case "done_gate_rebased":
		if m := shaArrowRe.FindStringSubmatch(ev.Data); m != nil {
			c.head = m[2]
		}
	case "done_gate_failed", "train_test_failed":
		c.train = store.TestFailed
	case "train_conflict":
		c.train = store.TrainError
	case "landed":
		c.train = store.TrainOK
		c.task.Status = store.Landed
		c.head = strings.TrimSpace(ev.Data)
	case "pr_layout":
		if m := layoutRe.FindStringSubmatch(ev.Data); m != nil {
			c.prOpen, c.prBase = true, m[1]
			sha := ""
			if i := strings.Index(ev.Data, "published as "); i >= 0 {
				sha, _, _ = strings.Cut(ev.Data[i+len("published as "):], " ")
			}
			c.push(sha, ev.TS)
		}
	case "restack_retarget", EventPRBase:
		if m := arrowRe.FindStringSubmatch(ev.Data); m != nil && c.prOpen {
			if m[3] != c.prBase {
				c.prBase = m[3]
				c.push("", ev.TS) // a new base: the old checks are the old base's
			}
		}
	case "restack":
		if c.train == store.TrainOK && !c.prOpen && !c.inBase {
			w.openPR(c, ev.TS)
		}
		if m := shaArrowRe.FindStringSubmatch(ev.Data); m != nil {
			c.push(m[2], ev.TS)
		}
	case "automerge_waiting", "automerge_refused":
		w.openPR(c, ev.TS)
	case "gh_stack_link":
		ids := linkSplit.Split(strings.TrimSpace(ev.Data), -1)
		for i, id := range ids {
			if l := w.change(id); l != nil {
				l.stackBelow = 0
				if i > 0 && w.change(ids[i-1]) != nil {
					l.stackBelow = w.change(ids[i-1]).pr
				}
			}
		}
		w.native = true
	case EventCIRedKind:
		for _, n := range ciNames(ev) {
			c.checks[n] = prgraph.CheckFailure
		}
	case "ci_recovered":
		for _, n := range ciNames(ev) {
			c.checks[n] = prgraph.CheckSuccess
		}
	case "ci_red_clear":
		for n, s := range c.checks {
			if s == prgraph.CheckFailure {
				c.checks[n] = prgraph.CheckSuccess
			}
		}
	case eventCIRepairSpawned:
		c.repairs++
	case "automerge_merged":
		w.merged(c)
	case "unstacked":
		switch {
		case strings.HasPrefix(ev.Data, "merged"):
			w.merged(c)
		case strings.HasPrefix(ev.Data, "folded"):
			c.train, c.prOpen = TrainFolded, false
		default:
			c.train, c.prOpen = TrainSuperseded, false
		}
	case "kill":
		c.task.Status = store.Killed
		if c.train == store.Queued || c.train == store.TestFailed || c.train == store.TrainError {
			c.train = TrainSuperseded
		}
	}
}

func (w *replayWorld) merged(c *rChange) {
	if !c.inBase {
		w.merges++
	}
	c.train, c.prOpen, c.inBase = TrainMerged, false, true
}

// stores is the world as today's stores and the forge would have shown it
// at now.
func (w *replayWorld) stores(now time.Time) (ShadowStores, prgraph.ForgeSnapshot, prgraph.GitFacts) {
	s := ShadowStores{Now: now, Base: w.base, Automerge: true, Since: w.hist.since, Policy: w.policy}
	snap := prgraph.ForgeSnapshot{NativeStacks: w.native}
	for n := range w.required {
		snap.Required = append(snap.Required, prgraph.RequiredCheck{Base: w.base, Name: n})
	}
	sort.Slice(snap.Required, func(i, j int) bool { return snap.Required[i].Name < snap.Required[j].Name })
	git := prgraph.GitFacts{Bases: []prgraph.BranchHead{{Branch: w.base, SHA: w.base + "@" + strconv.Itoa(w.merges)}}}
	for _, id := range sortedKeys(w.changes) {
		c := w.changes[id]
		t := c.task
		t.PR = "#" + strconv.Itoa(c.pr)
		if !c.prOpen && c.train != TrainMerged && c.train != TrainSuperseded {
			t.PR = ""
		}
		s.Tasks = append(s.Tasks, t)
		s.Train = append(s.Train, store.TrainEntry{Task: id, Seq: c.seq, State: c.train, Note: c.head})
		git.Heads = append(git.Heads, prgraph.BranchHead{Branch: t.Branch, SHA: c.head})
		if c.inBase {
			git.InBase = append(git.InBase, id)
		}
		var failing, host []string
		for _, n := range sortedKeys(c.checks) {
			if c.checks[n] == prgraph.CheckFailure {
				if n == baseStatus {
					host = append(host, n)
				} else {
					failing = append(failing, n)
				}
			}
		}
		if len(failing)+len(host) > 0 {
			s.CIRed.Red = append(s.CIRed.Red, CIRedLayer{Task: id, Head: c.head, Checks: append(failing, host...)})
		}
		for range c.repairs {
			s.CIRed.Repairs = append(s.CIRed.Repairs, CIRepairRecord{Task: id, Head: c.head, State: ciRepairSpawned})
		}
		if !c.prOpen {
			continue
		}
		snap.PRs = append(snap.PRs, prgraph.PR{Number: c.pr, Branch: t.Branch, Head: c.head, Base: c.prBase,
			State: prgraph.PROpen, Mergeable: prgraph.MergeClean, StackBelow: c.stackBelow})
		ran := c.pushedAt.IsZero() || now.Sub(c.pushedAt) >= ShadowCIRun
		for _, n := range sortedKeys(w.required) {
			st, ok := c.checks[n]
			if !ok {
				if !ran {
					continue // pending
				}
				st = prgraph.CheckSuccess
			}
			snap.Checks = append(snap.Checks, prgraph.Check{Head: c.head, Name: n, Status: st, Required: true})
		}
		if st, ok := c.checks[baseStatus]; ok {
			snap.Checks = append(snap.Checks, prgraph.Check{Head: c.head, Name: baseStatus, Status: st, Required: true, HostOwned: true})
		}
	}
	for _, k := range sortedKeys(w.edges) {
		s.Edges = append(s.Edges, w.edges[k])
	}
	return s, snap, git
}

func (w *replayWorld) plan(now time.Time) prgraph.Plan {
	s, snap, git := w.stores(now)
	_, p := SettlePlan(ShadowInput(s, snap, git))
	return p
}

// run replays the events and judges the window.
func (w *replayWorld) run(o ReplayOptions) ReplayReport {
	rep := ReplayReport{From: o.From, Until: o.Until, Assumptions: replayAssumptions}
	d := newShadowDiff(o.Window)
	fixes := map[string]bool{}
	seen := map[string]bool{}
	observe := func(at time.Time, legacy []LegacyAction) {
		p := w.plan(at)
		rep.Reconciles++
		for _, a := range p.Actions {
			if a.Kind == prgraph.ActNeedsFix && a.Change != "" {
				fixes[a.Change] = true
			}
		}
		rep.Divergences = append(rep.Divergences, d.Observe(at, p, legacy)...)
	}
	next := o.From
	for _, ev := range w.events {
		in := !ev.TS.Before(o.From)
		for in && !next.After(ev.TS) {
			observe(next, nil)
			next = next.Add(o.Tick)
		}
		la, isAction := w.pre(ev)
		if in {
			rep.Events++
			switch ev.Kind {
			case eventCIRepairSpawned:
				rep.Outcomes.LegacyRepairs++
				if c := w.change(ev.Task); c != nil && c.checks[baseStatus] == prgraph.CheckFailure && onlyHostRed(c) {
					rep.Outcomes.LegacyHostRepairs++
				}
			case "stack_at_risk":
				rep.Outcomes.AtRiskSpells++
			}
			if w.change(ev.Task) != nil {
				seen[ev.Task] = true
			}
			if isAction {
				if w.change(la.Change) == nil {
					rep.Unknown++
				} else {
					observe(ev.TS, []LegacyAction{la})
				}
			}
		}
		w.post(ev)
		if in && w.change(ev.Task) != nil {
			seen[ev.Task] = true
		}
	}
	for ; next.Before(o.Until); next = next.Add(o.Tick) {
		observe(next, nil)
	}
	rep.Divergences = append(rep.Divergences, d.Flush(o.Until)...)
	sort.SliceStable(rep.Divergences, func(i, j int) bool { return rep.Divergences[i].At.Before(rep.Divergences[j].At) })
	rep.Outcomes.PrgraphFixes = len(fixes)
	rep.Outcomes.Changes = len(seen)
	for id := range seen {
		if w.changes[id].inBase {
			rep.Outcomes.Merged++
		}
	}
	return rep
}

// onlyHostRed reports whether c's only failing checks are host-owned.
func onlyHostRed(c *rChange) bool {
	for n, s := range c.checks {
		if s == prgraph.CheckFailure && n != baseStatus {
			return false
		}
	}
	return true
}
