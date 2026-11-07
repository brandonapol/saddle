package app

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/prgraph"
	"github.com/brandonapol/saddle/internal/store"
)

// The shadow reconciler (#830, ADR 0009 §6.2). An adapter builds prgraph's
// Input from today's stores; Reconcile runs on it every sentinel tick, and
// the actions prgraph would take that saddle didn't, and the ones saddle
// took that prgraph wouldn't, are logged as reconcile_shadow events. The
// shadow never acts: it has no executor, and nothing here writes to git,
// the forge or saddle's own state files.

// EventReconcileShadow is the events-table kind for one divergence between
// prgraph's plan and what saddle's own code did.
const EventReconcileShadow = "reconcile_shadow"

// Divergence sides.
const (
	// ShadowPrgraphOnly: prgraph planned the action and saddle didn't take
	// it within the shadow's window.
	ShadowPrgraphOnly = "prgraph_only"
	// ShadowLegacyOnly: saddle took the action and prgraph's plan didn't
	// have it.
	ShadowLegacyOnly = "legacy_only"
)

// ShadowWindow is how long a planned action may wait for saddle to take it
// before the shadow calls it prgraph's alone: about five sentinel ticks.
const ShadowWindow = 10 * time.Minute

// shadowHold is how long an owner's queue hold counts as a hold for prgraph
// (ADR 0009: holds always have a time, 24h at most).
const shadowHold = 24 * time.Hour

// ShadowStores is what today's stores say about the stack, read once per
// tick: the train table, tasks, deps/<task> edges, stacks.json,
// ci-red.json, automerge.json's holds and published.json. The pure adapter
// ShadowInput maps it onto prgraph's desired state.
type ShadowStores struct {
	Now   time.Time
	Base  string
	Tasks []store.Task
	Train []store.TrainEntry
	// Edges are the after edges (deps/<task> and depends_on events).
	Edges  []prgraph.Edge
	Stacks []CustomStack
	CIRed  CIRedState
	// Automerge is whether auto-merge is on; AutomergeHolds are the held
	// tasks or PRs.
	Automerge      bool
	AutomergeHolds []string
	// Published maps tasks published on their own (published.json).
	Published map[string]publishedPR
	// Since is when each task's legacy state began, when known. It is only
	// the change's since: the shadow never seeds a deadline. Legacy's timing
	// isn't prgraph's (prgraph would have published or merged earlier), so a
	// deadline fired by an action prgraph planned but legacy never took
	// would cascade into reds that only the shadow sees.
	Since  map[string]time.Time
	Policy prgraph.Policy
}

var prNumRe = regexp.MustCompile(`(?:/pull/|/merge_requests/|^#?)(\d+)\s*$`)

// prNumberOf is the PR number in a PR URL (or "#n"), 0 when there is none.
func prNumberOf(ref string) int {
	m := prNumRe.FindStringSubmatch(strings.TrimSpace(ref))
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// noteHead is the last commit of a train note's landed range.
func noteHead(note string) string {
	if _, to, ok := strings.Cut(note, ".."); ok {
		return to
	}
	if strings.ContainsAny(note, " :") {
		return ""
	}
	return note
}

// ShadowInput is the adapter: it maps today's stores onto prgraph's State
// and pairs it with the actual forge and git state. Merged and superseded
// train entries are left out unless a live change depends on them or their
// PR is still open; folded repairs are never changes (a repair is a new
// head of the change it repairs).
func ShadowInput(s ShadowStores, forge prgraph.ForgeSnapshot, git prgraph.GitFacts) prgraph.Input {
	p := s.Policy
	if !slices.Contains(p.HostChecks, baseStatus) {
		p.HostChecks = append(append([]string(nil), p.HostChecks...), baseStatus)
	}
	byID := make(map[string]store.Task, len(s.Tasks))
	for _, t := range s.Tasks {
		byID[t.ID] = t
	}
	heads := map[string]string{}
	for _, h := range git.Heads {
		heads[h.Branch] = h.SHA
	}
	openBranch := map[string]bool{}
	for _, pr := range forge.PRs {
		if pr.State == prgraph.PROpen {
			openBranch[pr.Branch] = true
		}
	}
	repairs := map[string]int{}
	for _, r := range s.CIRed.Repairs {
		if r.State != ciRepairPending {
			repairs[r.Task]++
		}
	}
	held := func(t store.Task) bool {
		return slices.Contains(s.AutomergeHolds, t.ID) || (t.PR != "" && slices.Contains(s.AutomergeHolds, t.PR))
	}
	var all []prgraph.Change
	legacyTerminal := map[string]bool{}
	for _, e := range s.Train {
		t, ok := byID[e.Task]
		if !ok || t.Role == store.RoleOrchestrator || t.Branch == "" || e.State == TrainFolded {
			continue
		}
		c := prgraph.Change{ID: t.ID, Branch: t.Branch, Base: s.Base, Seq: e.Seq, Gen: 1,
			Automerge: s.Automerge && !held(t), PR: prNumberOf(t.PR)}
		if c.PR == 0 {
			c.PR = prNumberOf(s.Published[t.ID].URL)
		}
		c.Head = heads[t.Branch]
		if c.Head == "" {
			c.Head = noteHead(e.Note)
		}
		switch e.State {
		case store.Queued:
			c.State = prgraph.Queued
		case store.OnHold:
			c.State, c.HoldUntil = prgraph.Queued, s.Now.Add(shadowHold)
		case store.TrainError:
			c.State, c.Reason = prgraph.Red, prgraph.CauseConflict
		case store.TestFailed, TrainEscalated:
			c.State, c.Reason = prgraph.Red, prgraph.CauseGate
		case TrainMerged:
			c.State = prgraph.Merged
			legacyTerminal[c.ID] = true
		case TrainSuperseded:
			c.State, c.Reason = prgraph.Superseded, prgraph.SupersededClosed
			if t.Status == store.Killed {
				c.Reason = prgraph.SupersededKilled
			}
			legacyTerminal[c.ID] = true
		default: // landed, repairing
			switch l, red := s.CIRed.Layer(t.ID); {
			case t.Status == store.Killed && c.PR != 0:
				c.State, c.Reason = prgraph.Superseded, prgraph.SupersededKilled
			case red && !l.Acked && len(l.Checks) > 0:
				c.State, c.Reason = prgraph.Red, prgraph.CheckCause(l.Checks[0])
			case c.PR != 0:
				c.State = prgraph.Published
			default:
				c.State = prgraph.Landed
			}
		}
		c.Since = s.Now
		if at, ok := s.Since[c.ID]; ok && !at.IsZero() {
			c.Since = at
		}
		all = append(all, c)
	}
	edges := append([]prgraph.Edge(nil), s.Edges...)
	for _, st := range s.Stacks {
		for i := 1; i < len(st.Tasks); i++ {
			edges = append(edges, prgraph.Edge{Change: st.Tasks[i], Dep: st.Tasks[i-1], Kind: prgraph.Declared, Source: "owner_stack"})
		}
	}
	// Keep every change legacy still has in play, and the finished ones a
	// live change depends on or whose PR is still open.
	keep := map[string]bool{}
	for _, c := range all {
		if !legacyTerminal[c.ID] || openBranch[c.Branch] {
			keep[c.ID] = true
		}
	}
	for _, e := range edges {
		if keep[e.Change] {
			keep[e.Dep] = true
		}
	}
	st := prgraph.State{Policy: p}
	for _, c := range all {
		if keep[c.ID] {
			st.Changes = append(st.Changes, c)
		}
	}
	for _, e := range edges {
		if keep[e.Change] && keep[e.Dep] {
			st.Edges = append(st.Edges, e)
		}
	}
	for _, id := range sortedKeys(repairs) {
		if keep[id] {
			st.Attempts = append(st.Attempts, prgraph.Attempt{Key: "needs_fix/" + id, Kind: string(prgraph.ActNeedsFix),
				N: repairs[id], Last: prgraph.OutcomeOK})
		}
	}
	return prgraph.Input{Now: s.Now, State: st, Forge: forge, Git: git}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// shadowSettleRounds bounds SettlePlan's loop.
const shadowSettleRounds = 8

// SettlePlan runs Reconcile and applies its Transitions to the input's
// state in memory until none is left, then returns the settled input and
// its plan. The adapter's states are legacy's, mapped; settling lets
// prgraph's own state machine take them where it would have (published to
// green, red back to published), so the plan is what prgraph would do next.
// Nothing outside the returned Input changes.
func SettlePlan(in prgraph.Input) (prgraph.Input, prgraph.Plan) {
	in.State.Changes = append([]prgraph.Change(nil), in.State.Changes...)
	for range shadowSettleRounds {
		p := prgraph.Reconcile(in)
		moved := false
		for _, a := range p.Actions {
			if a.Kind == prgraph.ActTransition && applyTransition(&in.State, a, in.Now) {
				moved = true
			}
		}
		if !moved {
			return in, p
		}
	}
	return in, prgraph.Reconcile(in)
}

// applyTransition is the store's CAS, in memory.
func applyTransition(st *prgraph.State, a prgraph.Action, now time.Time) bool {
	heads := map[string]prgraph.Change{}
	for _, c := range st.Changes {
		heads[c.ID] = c
	}
	for _, d := range a.Guard.Deps {
		id, head, _ := strings.Cut(d, "=")
		if c, ok := heads[id]; !ok || c.Head != head || c.State.Terminal() {
			return false
		}
	}
	for i := range st.Changes {
		c := &st.Changes[i]
		if c.ID != a.Change || (a.Guard.Gen != 0 && c.Gen != a.Guard.Gen) {
			continue
		}
		c.State, c.Reason, c.Since, c.Deadline, c.RetryAt = a.To, a.Reason, now, a.Deadline, a.RetryAt
		if a.Head != "" {
			c.Head = a.Head
		}
		if a.PR != 0 {
			c.PR = a.PR
		}
		c.Gen++
		return true
	}
	return false
}

// LegacyAction is one thing saddle's own code did, read from an event, as
// the prgraph action it corresponds to.
type LegacyAction struct {
	prgraph.Action
	At     time.Time
	Detail string // the event it came from
}

// ShadowDivergence is one difference between prgraph's plan and saddle.
type ShadowDivergence struct {
	At     time.Time          `json:"at"`
	Side   string             `json:"side"` // ShadowPrgraphOnly or ShadowLegacyOnly
	Kind   prgraph.ActionKind `json:"kind"`
	Change string             `json:"change,omitempty"`
	Action string             `json:"action"`
	Detail string             `json:"detail,omitempty"`
}

// Line is the divergence as one event line.
func (d ShadowDivergence) Line() string {
	s := d.Side + " " + d.Action
	if d.Detail != "" {
		s += ": " + d.Detail
	}
	return s
}

// shadowCompared are the actions the diff compares. Transitions are
// bookkeeping (the adapter's states are legacy's), and a wait does nothing.
func shadowCompared(k prgraph.ActionKind) bool {
	switch k {
	case prgraph.ActTransition, prgraph.ActWait:
		return false
	}
	return true
}

// shadowKey identifies a planned action across ticks: its kind and target,
// not its guard or onto (which move with every new base tip).
func shadowKey(a prgraph.Action) string {
	return fmt.Sprintf("%s|%s|%d|%s|%s", a.Kind, a.Change, a.PR, a.Base, a.Stack)
}

// sameAction reports whether legacy's action is the planned one.
func sameAction(legacy, planned prgraph.Action) bool {
	if legacy.Kind != planned.Kind {
		return false
	}
	if legacy.Kind == prgraph.ActLink {
		return legacy.Stack == planned.Stack
	}
	if legacy.Change != planned.Change && (legacy.PR == 0 || legacy.PR != planned.PR) {
		return false
	}
	return legacy.Base == "" || planned.Base == "" || legacy.Base == planned.Base
}

// shadowDiff compares a stream of plans with a stream of legacy actions.
type shadowDiff struct {
	window  time.Duration
	last    []prgraph.Action
	pending map[string]pendingAction // planned, not yet taken by legacy
	seen    map[string]bool          // planned and already accounted for
}

type pendingAction struct {
	a     prgraph.Action
	first time.Time
}

func newShadowDiff(window time.Duration) *shadowDiff {
	return &shadowDiff{window: window, pending: map[string]pendingAction{}, seen: map[string]bool{}}
}

// Observe takes the plan at now and the legacy actions since the last call.
// A legacy action no plan of the window has is reported at once; a planned
// action legacy hasn't taken within the window is reported once.
func (d *shadowDiff) Observe(now time.Time, plan prgraph.Plan, legacy []LegacyAction) []ShadowDivergence {
	var cur []prgraph.Action
	for _, a := range plan.Actions {
		if shadowCompared(a.Kind) {
			cur = append(cur, a)
		}
	}
	var out []ShadowDivergence
	for _, la := range legacy {
		if d.match(la.Action, cur) {
			continue
		}
		out = append(out, ShadowDivergence{At: la.At, Side: ShadowLegacyOnly, Kind: la.Kind, Change: la.Change,
			Action: la.String(), Detail: la.Detail})
	}
	present := map[string]bool{}
	for _, a := range cur {
		k := shadowKey(a)
		present[k] = true
		if !d.seen[k] {
			d.seen[k] = true
			d.pending[k] = pendingAction{a: a, first: now}
		}
	}
	for _, k := range sortedKeys(d.pending) {
		p := d.pending[k]
		if now.Sub(p.first) >= d.window {
			out = append(out, d.prgraphOnly(p, now))
			delete(d.pending, k)
		}
	}
	for k := range d.seen {
		if !present[k] {
			if _, ok := d.pending[k]; !ok {
				delete(d.seen, k)
			}
		}
	}
	d.last = cur
	return out
}

func (d *shadowDiff) prgraphOnly(p pendingAction, now time.Time) ShadowDivergence {
	return ShadowDivergence{At: p.first, Side: ShadowPrgraphOnly, Kind: p.a.Kind, Change: p.a.Change, Action: p.a.String(),
		Detail: fmt.Sprintf("planned for %s and saddle didn't take it", now.Sub(p.first).Round(time.Minute))}
}

// match finds la in the current plan, the last one or the pending actions,
// and settles the pending one it matches.
func (d *shadowDiff) match(la prgraph.Action, cur []prgraph.Action) bool {
	for _, k := range sortedKeys(d.pending) {
		if sameAction(la, d.pending[k].a) {
			delete(d.pending, k)
			return true
		}
	}
	for _, a := range append(append([]prgraph.Action(nil), cur...), d.last...) {
		if sameAction(la, a) {
			delete(d.pending, shadowKey(a))
			d.seen[shadowKey(a)] = true
			return true
		}
	}
	return false
}

// Flush reports every planned action still waiting for legacy, as of now.
func (d *shadowDiff) Flush(now time.Time) []ShadowDivergence {
	var out []ShadowDivergence
	for _, k := range sortedKeys(d.pending) {
		out = append(out, d.prgraphOnly(d.pending[k], now))
		delete(d.pending, k)
	}
	return out
}

var (
	arrowRe    = regexp.MustCompile(`^(\S+?):?\s+(?:(\S+)\s+)?→\s+(\S+)`)
	layoutRe   = regexp.MustCompile(`published as \S+ on (\S+)`)
	linkSplit  = regexp.MustCompile(`\s*→\s*`)
	detailSize = 160
)

// legacyAction reads one event as the action saddle's own code took, if it
// was one. prOf holds the tasks that already had a PR before ev: a layout
// push onto an open PR is a rebase, not a publish.
func legacyAction(ev store.Event, prOf map[string]int) (LegacyAction, bool) {
	detail := ev.Kind + " " + strings.ReplaceAll(ev.Data, "\n", " | ")
	if len(detail) > detailSize {
		detail = detail[:detailSize] + "…"
	}
	la := LegacyAction{At: ev.TS, Detail: detail, Action: prgraph.Action{Change: ev.Task}}
	switch ev.Kind {
	case eventCIRepairSpawned:
		la.Kind = prgraph.ActNeedsFix
	case "train_test_failed":
		la.Kind, la.Cause = prgraph.ActNeedsFix, prgraph.CauseGate
	case "train_conflict":
		la.Kind, la.Cause = prgraph.ActNeedsFix, prgraph.CauseConflict
	case "done_gate_started":
		la.Kind = prgraph.ActGate
	case "restack":
		la.Kind = prgraph.ActRebase
	case "restack_retarget", EventPRBase:
		m := arrowRe.FindStringSubmatch(ev.Data)
		if m == nil || m[2] == "" || m[2] == m[3] {
			return la, false // saddle set a base it already had
		}
		la.Kind, la.PR, la.Base = prgraph.ActMove, prNumberOf(m[1]), m[3]
	case "automerge_merged":
		url, _, _ := strings.Cut(ev.Data, " ")
		la.Kind, la.PR = prgraph.ActMerge, prNumberOf(url)
	case "pr_layout":
		m := layoutRe.FindStringSubmatch(ev.Data)
		if m == nil {
			return la, false
		}
		if prOf[ev.Task] != 0 {
			la.Kind, la.PR = prgraph.ActRebase, prOf[ev.Task]
		} else {
			la.Kind, la.Base = prgraph.ActPublish, m[1]
		}
	case "gh_stack_link":
		ids := linkSplit.Split(strings.TrimSpace(ev.Data), -1)
		if len(ids) == 0 || ids[0] == "" {
			return la, false
		}
		la.Kind, la.Change, la.Stack = prgraph.ActLink, ids[0], ids[0]
	default:
		return la, false
	}
	return la, true
}
