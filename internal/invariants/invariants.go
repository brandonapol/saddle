// Package invariants checks the invariants of ADR 0004 (#820) against one
// snapshot of saddle's state, git and the forge (#825). Check is pure: the
// app gathers a Snapshot each sentinel tick, and the monitor turns each new
// violation into one event and one notice. It only detects; healing comes
// with the reconciler (#829–#835), whose shadow (#830) can reuse Check.
package invariants

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/claims"
	"github.com/brandonapol/saddle/internal/usage"
)

// Kind names an invariant's violation, as in ADR 0004's table.
type Kind string

// The checks, one per invariant the monitor detects. I6 and I9–I11 are
// checked elsewhere (soak, contract tests, watcher heartbeats).
const (
	DoneNotOnMain    Kind = "done_not_on_main"   // I1
	PRDrift          Kind = "pr_drift"           // I2
	PRBaseWrong      Kind = "pr_base_wrong"      // I3
	Latched          Kind = "latched"            // I4
	StateWithoutExit Kind = "state_without_exit" // I5
	SelfRepair       Kind = "self_repair"        // I7
	PolicyBypass     Kind = "policy_bypass"      // I8
)

// DoneGrace is how long a task past the done gate may go without a PR before
// I1 calls it a violation: land opens the PR right after the train.
const DoneGrace = 10 * time.Minute

// StateSlack is how far past its deadline a state may be before I5 calls
// it stuck: the tick that runs its exit may not have come round yet.
const StateSlack = 10 * time.Minute

// Violation is one broken invariant on one subject. Kind and Subject are
// its identity; Detail says what is wrong now and may change between ticks.
type Violation struct {
	Kind    Kind   `json:"kind"`
	Subject string `json:"subject"`
	Detail  string `json:"detail"`
}

// Key is the violation's identity: one event and one notice per key until
// it clears.
func (v Violation) Key() string { return string(v.Kind) + " " + v.Subject }

func (v Violation) String() string { return v.Key() + ": " + v.Detail }

// Snapshot is what Check reads. The app fills it from state.db, the JSON
// stores, git and one forge read.
type Snapshot struct {
	Now  time.Time
	Base string // the branch work is done on (main)

	// Landed are the tasks past the done gate that are still in the stack.
	Landed []Landed

	// PRsKnown says the forge was read this tick; without it, PRs aren't
	// compared (I2, I3).
	PRsKnown bool
	PRs      []PR
	// Unmerged maps every task whose work isn't on base yet to its branch.
	Unmerged map[string]string

	Latches []Latch
	Tasks   []Task
	States  []State
	Repairs []Repair
	Spawns  []Spawn
	Policy  Policy
}

// Landed is a task the train landed.
type Landed struct {
	Task     string
	LandedAt time.Time // zero when unknown
	PR       string    // "" when it has none
	OnBase   bool      // base already has its work
}

// Builds is what the pre-publish gate knows about a PR's head on its base.
type Builds string

const (
	BuildsUnknown Builds = ""
	BuildsYes     Builds = "yes"
	BuildsNo      Builds = "no"
)

// PR is a stacked task's PR: what saddle's stores say and what the forge
// says.
type PR struct {
	Task, URL, Branch string
	SavedBase         string // the base saddle last set; "" when it set none
	LiveBase          string // the forge's base
	Open              bool   // the forge lists it open
	Deps              []string
	Builds            Builds
}

// Latch is a stop: something that halted and waits. RetryAt is when it is
// tried again, zero when nothing will.
type Latch struct {
	What, Subject, Detail string
	RetryAt               time.Time
}

// Task is a task's status, and when it was last killed and last brought
// back on purpose (requeue, resume).
type Task struct {
	ID, Status           string
	Live                 bool
	KilledAt, ReturnedAt time.Time
}

// State is a row that should have an exit: a card, a hold, a train row. A
// zero Deadline means it has no timeout. State is a train row's state; a
// terminal one is never a violation.
type State struct {
	What, Subject, Detail string
	State                 string
	Deadline              time.Time
}

// TerminalTrainState reports whether a train row's state is terminal: base
// has the work (merged), it shipped in another PR (folded), or it never
// will (superseded). A terminal state has no exit by definition (ADR 0009,
// docs/GLOSSARY.md), so I5 never flags one (#825). These mirror
// app.TrainMerged, app.TrainFolded and app.TrainSuperseded.
func TerminalTrainState(state string) bool {
	switch state {
	case "merged", "folded", "superseded":
		return true
	}
	return false
}

// Repair is a live repair task and the checks it was spawned for.
type Repair struct {
	Repair, For string
	Checks      []string
	RedOnBase   []string // the checks that are red on base too
}

// Spawn is how a live task was spawned.
type Spawn struct {
	Task, Adapter, Model string
	Claims               []string
}

// Policy is the configured spawn policy.
type Policy struct {
	Adapters      []string            // the adapters spawns may use; empty is any
	ExcludeModels map[string][]string // per adapter
	Serial        []string            // globs only the merge train changes
}

// SaddleCheck reports whether check is a status saddle itself sets, such as
// saddle/base.
func SaddleCheck(check string) bool { return strings.HasPrefix(check, "saddle/") }

// Check returns every violation in s, sorted by key.
func Check(s Snapshot) []Violation {
	var out []Violation
	add := func(k Kind, subject, format string, args ...any) {
		out = append(out, Violation{Kind: k, Subject: subject, Detail: fmt.Sprintf(format, args...)})
	}
	base := s.Base
	if base == "" {
		base = "base"
	}

	// I1
	for _, l := range s.Landed {
		if l.OnBase || l.PR != "" {
			continue
		}
		if !l.LandedAt.IsZero() && s.Now.Sub(l.LandedAt) < DoneGrace {
			continue
		}
		when := "at an unknown time"
		if !l.LandedAt.IsZero() {
			when = s.Now.Sub(l.LandedAt).Round(time.Minute).String() + " ago"
		}
		add(DoneNotOnMain, l.Task, "passed the done gate and landed %s, but its work isn't on %s and it has no PR", when, base)
	}

	// I2, I3
	if s.PRsKnown {
		branchTask := map[string]string{}
		for id, b := range s.Unmerged {
			branchTask[b] = id
		}
		for _, p := range s.PRs {
			if !p.Open {
				add(PRDrift, p.Task, "saddle has %s open in its stack, but the forge doesn't list it open", p.URL)
				continue
			}
			if p.SavedBase != "" && p.SavedBase != p.LiveBase {
				add(PRDrift, p.Task, "saddle set %s's base to %s, but the forge has %s", p.URL, p.SavedBase, p.LiveBase)
			}
			if why := wrongBase(p, s.Base, s.Unmerged, branchTask); why != "" {
				add(PRBaseWrong, p.Task, "%s targets %s: %s", p.URL, p.LiveBase, why)
			}
		}
	}

	// I4
	for _, l := range s.Latches {
		if l.RetryAt.IsZero() {
			add(Latched, l.What+" "+l.Subject, "%s, and nothing will retry it", l.Detail)
		}
	}

	// I5
	for _, t := range s.Tasks {
		if t.Live && !t.KilledAt.IsZero() && t.KilledAt.After(t.ReturnedAt) {
			add(StateWithoutExit, "task "+t.ID, "was killed at %s but is %s again with no requeue or resume", t.KilledAt.UTC().Format(time.RFC3339), t.Status)
		}
	}
	for _, st := range s.States {
		if TerminalTrainState(st.State) {
			continue
		}
		subject := st.What + " " + st.Subject
		detail := st.Detail
		if detail != "" {
			detail += "; "
		}
		switch {
		case st.Deadline.IsZero():
			add(StateWithoutExit, subject, "%shas no timeout, so only a human can end it", detail)
		case s.Now.Sub(st.Deadline) > StateSlack:
			add(StateWithoutExit, subject, "%swas due to exit %s ago", detail, s.Now.Sub(st.Deadline).Round(time.Minute))
		}
	}

	// I7
	for _, r := range s.Repairs {
		var own []string
		for _, c := range r.Checks {
			if SaddleCheck(c) {
				own = append(own, c)
			}
		}
		switch {
		case len(own) > 0:
			add(SelfRepair, r.Repair, "repairs %s for %s, a check saddle sets itself", r.For, strings.Join(own, ", "))
		case len(r.RedOnBase) > 0:
			add(SelfRepair, r.Repair, "repairs %s for %s, which is red on base too", r.For, strings.Join(r.RedOnBase, ", "))
		}
	}

	// I8
	for _, sp := range s.Spawns {
		var why []string
		if len(s.Policy.Adapters) > 0 && sp.Adapter != "" && !slices.Contains(s.Policy.Adapters, sp.Adapter) {
			why = append(why, fmt.Sprintf("adapter %s isn't in the allowed adapters (%s)", sp.Adapter, strings.Join(s.Policy.Adapters, ", ")))
		}
		for _, x := range s.Policy.ExcludeModels[sp.Adapter] {
			if usage.ModelIs(sp.Model, x) {
				why = append(why, fmt.Sprintf("model %s is excluded for %s", sp.Model, sp.Adapter))
				break
			}
		}
		for _, c := range sp.Claims {
			if i := slices.IndexFunc(s.Policy.Serial, func(g string) bool { return claims.Overlap(c, g) }); i >= 0 {
				why = append(why, fmt.Sprintf("claim %s covers serial %s", c, s.Policy.Serial[i]))
			}
		}
		if len(why) > 0 {
			add(PolicyBypass, sp.Task, "spawned against the policy: %s", strings.Join(why, "; "))
		}
	}

	slices.SortStableFunc(out, func(a, b Violation) int { return strings.Compare(a.Key(), b.Key()) })
	return out
}

// wrongBase says why p's live base isn't its nearest unmerged dependency,
// "" when it may be right. It flags what it can tell for sure: base while
// a dependency hasn't merged, a branch no unmerged task owns, and a head the
// pre-publish gate saw fail there.
func wrongBase(p PR, base string, unmerged map[string]string, branchTask map[string]string) string {
	var deps []string
	for _, d := range p.Deps {
		if _, ok := unmerged[d]; ok && d != p.Task {
			deps = append(deps, d)
		}
	}
	switch {
	case p.LiveBase == base && len(deps) > 0:
		return "it depends on " + strings.Join(deps, ", ") + ", which isn't on " + base + " yet"
	case p.LiveBase != base && p.LiveBase != "" && branchTask[p.LiveBase] == "":
		return "no unmerged task owns that branch any more"
	case p.Builds == BuildsNo:
		return "its head doesn't build there"
	}
	return ""
}

// Diff splits now against the open violations: fresh are keys not open yet,
// cleared are open keys no longer violated.
func Diff(open map[string]Violation, now []Violation) (fresh, cleared []Violation) {
	seen := map[string]bool{}
	for _, v := range now {
		seen[v.Key()] = true
		if _, ok := open[v.Key()]; !ok {
			fresh = append(fresh, v)
		}
	}
	for k, v := range open {
		if !seen[k] {
			cleared = append(cleared, v)
		}
	}
	slices.SortFunc(cleared, func(a, b Violation) int { return strings.Compare(a.Key(), b.Key()) })
	return fresh, cleared
}
