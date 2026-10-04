package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// A stack built on failing CI is bad (#213). The ci-red watcher (in
// internal/sentinel) polls the checks of every stacked PR. A PR whose checks
// failed on its head marks its layer red, and every layer above it in its PR
// stack is held: prs neither pushes them nor opens new PRs there, land holds
// queued work that changes the files those layers changed, and auto-merge
// merges none of them. Holding is routine, not a human decision: the PRs
// carry the ci-red label and the orchestrator hears an info notice. The hold
// lifts by itself once the red layer's checks pass. `saddle sentinel ack`
// acknowledges it and `saddle unstack` drops the red task, as for the
// at-risk flag (#119).

// LabelCIRed marks a red PR and the PRs held above it.
const LabelCIRed = "ci-red"

// CIRedLayer is one stacked task whose PR's checks failed on its head.
type CIRedLayer struct {
	Task   string    `json:"task"`
	PR     string    `json:"pr"`
	Head   string    `json:"head"`             // the head commit the checks failed on
	Checks []string  `json:"checks"`           // the failing checks' labels
	Since  time.Time `json:"since"`            // when it first went red
	Held   []string  `json:"held,omitempty"`   // stacked tasks above it in its PR stack
	Acked  bool      `json:"acked,omitempty"`  // sentinel ack: it holds nothing back
	Repair string    `json:"repair,omitempty"` // the live repair task, if any
	// Escalated is set once the orchestrator was told the repairs ran out.
	Escalated bool `json:"escalated,omitempty"`
}

// Repair record states.
const (
	ciRepairSpawned = "spawned" // the repair task is working, or landed and not yet folded
	ciRepairFolded  = "folded"  // its commits joined the red layer
	ciRepairFailed  = "failed"  // it could not be folded into the red layer
)

// CIRepairRecord is one repair spawned for a red layer (see cired_repair.go).
type CIRepairRecord struct {
	Task   string `json:"task"`   // the red task
	Head   string `json:"head"`   // the red head it was spawned for
	Repair string `json:"repair"` // the repair task
	Fixer  string `json:"fixer,omitempty"`
	State  string `json:"state"` // spawned, folded or failed
	Note   string `json:"note,omitempty"`
}

// CIRedState is what the ci-red watcher knows, saved under .saddle.
type CIRedState struct {
	Red []CIRedLayer `json:"red,omitempty"`
	// Labeled are the PRs that carry the ci-red label.
	Labeled []string `json:"labeled,omitempty"`
	// Repairs are the repairs spawned since each layer was last green.
	Repairs []CIRepairRecord `json:"repairs,omitempty"`
	// Explained are red heads a pending sibling PR explains (#193), as
	// "task@head", so they are reported once.
	Explained []string `json:"explained,omitempty"`
}

// Layer is the red layer of task, if it is red.
func (s CIRedState) Layer(task string) (CIRedLayer, bool) {
	i := slices.IndexFunc(s.Red, func(l CIRedLayer) bool { return l.Task == task })
	if i < 0 {
		return CIRedLayer{}, false
	}
	return s.Red[i], true
}

// holding lists the red layers that hold work back: not acked.
func (s CIRedState) holding() []CIRedLayer {
	var out []CIRedLayer
	for _, l := range s.Red {
		if !l.Acked {
			out = append(out, l)
		}
	}
	return out
}

func (a *App) ciRedPath() string { return a.stateDir("ci-red.json") }

// CIRed reads the ci-red state; empty when nothing was ever red. It asks
// GitHub nothing, so the TUI can read it every frame.
func (a *App) CIRed() (CIRedState, error) {
	var s CIRedState
	b, err := os.ReadFile(a.ciRedPath())
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("%s: %w", a.ciRedPath(), err)
	}
	return s, nil
}

// SetCIRed saves the ci-red state.
func (a *App) SetCIRed(s CIRedState) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.ciRedPath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.ciRedPath())
}

// CIPoll is what one poll saw on a stacked PR.
type CIPoll struct {
	Task   string
	PR     string
	Head   string
	Failed []string // failing checks on Head; empty when none failed
	Green  bool     // every check on Head passed (or it has none)
}

// CIRedTarget is a stacked task with a PR, for the watcher to poll.
type CIRedTarget struct {
	Task, Branch, PR string
}

// CIRedTargets lists the stacked tasks that have PRs, in train order.
func (a *App) CIRedTargets() ([]CIRedTarget, error) {
	stack, err := a.landedStack()
	if err != nil {
		return nil, err
	}
	var out []CIRedTarget
	for _, l := range stack {
		if l.PR != "" {
			out = append(out, CIRedTarget{Task: l.ID, Branch: l.Branch, PR: l.PR})
		}
	}
	return out, nil
}

// CIRedChange is what ApplyCIRed changed.
type CIRedChange struct {
	Red     []CIRedLayer // layers that went red, or red again on a new head
	Cleared []CIRedLayer // layers that went green or left the stack
	State   CIRedState   // the state now
}

// ApplyCIRed folds one poll of every stacked PR into the ci-red state and
// saves it: a failed check on a PR's head makes its layer red, a green head
// clears it, a pending one keeps what was known. A layer that left the stack
// clears too. It recomputes what each red layer holds.
func (a *App) ApplyCIRed(polls []CIPoll) (CIRedChange, error) {
	var ch CIRedChange
	s, err := a.CIRed()
	if err != nil {
		return ch, err
	}
	stack, err := a.landedStack()
	if err != nil {
		return ch, err
	}
	stacked := map[string]bool{}
	for _, l := range stack {
		stacked[l.ID] = true
	}
	seen := map[string]CIPoll{}
	for _, p := range polls {
		seen[p.Task] = p
	}
	now := time.Now().UTC()
	var next []CIRedLayer
	for _, l := range s.Red {
		p, polled := seen[l.Task]
		switch {
		case !stacked[l.Task], polled && p.Green:
			ch.Cleared = append(ch.Cleared, l)
		case polled && len(p.Failed) > 0 && p.Head != l.Head:
			l.Head, l.Checks, l.PR, l.Acked = p.Head, p.Failed, p.PR, false
			ch.Red = append(ch.Red, l)
			next = append(next, l)
		default:
			next = append(next, l)
		}
	}
	for _, p := range polls {
		if len(p.Failed) == 0 || !stacked[p.Task] || slices.ContainsFunc(next, func(l CIRedLayer) bool { return l.Task == p.Task }) {
			continue
		}
		l := CIRedLayer{Task: p.Task, PR: p.PR, Head: p.Head, Checks: p.Failed, Since: now}
		ch.Red = append(ch.Red, l)
		next = append(next, l)
	}
	// A green layer's repair history starts over.
	for _, c := range ch.Cleared {
		s.Repairs = slices.DeleteFunc(s.Repairs, func(r CIRepairRecord) bool { return r.Task == c.Task && r.State != ciRepairSpawned })
	}
	s.Red = next
	if len(s.Red) > 0 {
		if err := a.ciRedHolds(stack, s.Red); err != nil {
			return ch, err
		}
	}
	ch.State = s
	return ch, a.SetCIRed(s)
}

// ciRedHolds fills in what each red layer holds: the stacked tasks above it
// in its PR stack.
func (a *App) ciRedHolds(stack []landedTask, red []CIRedLayer) error {
	layout, _, err := a.stackLayout(stack)
	if err != nil {
		return err
	}
	for i := range red {
		red[i].Held = nil
		at := slices.IndexFunc(stack, func(l landedTask) bool { return l.ID == red[i].Task })
		if at < 0 {
			continue
		}
		for j := range stack {
			if j != at && above(layout, j, at) {
				red[i].Held = append(red[i].Held, stack[j].ID)
			}
		}
	}
	return nil
}

// above reports whether layer j's PR sits on layer at, directly or through
// the layers between.
func above(layout []prLayer, j, at int) bool {
	for b, n := layout[j].Below, 0; b >= 0 && n <= len(layout); b, n = layout[b].Below, n+1 {
		if b == at {
			return true
		}
	}
	return false
}

// ciRedHeld is the publish hook: of landed, laid out as layout, the layers
// prs must leave alone because a red layer is below them, each with why.
// The red layer itself is published, so its repair reaches its PR.
func (a *App) ciRedHeld(landed []landedTask, layout []prLayer) map[int]string {
	s, err := a.CIRed()
	if err != nil {
		return nil
	}
	out := map[int]string{}
	for _, r := range s.holding() {
		at := slices.IndexFunc(landed, func(l landedTask) bool { return l.ID == r.Task })
		if at < 0 {
			continue
		}
		for j := range landed {
			if _, ok := out[j]; !ok && j != at && above(layout, j, at) {
				out[j] = r.Task
			}
		}
	}
	return out
}

// ciRedErr explains layers prs held for red CI.
func ciRedErr(held map[int]string, landed []landedTask) error {
	if len(held) == 0 {
		return nil
	}
	by := map[string][]string{}
	var reds []string
	for j, r := range held {
		if _, ok := by[r]; !ok {
			reds = append(reds, r)
		}
		by[r] = append(by[r], landed[j].ID)
	}
	slices.Sort(reds)
	var parts []string
	for _, r := range reds {
		slices.Sort(by[r])
		parts = append(parts, fmt.Sprintf("%s (held: %s)", r, strings.Join(by[r], ", ")))
	}
	return fmt.Errorf("CI is red on %s, so prs pushed nothing above it and opened no PR there. "+
		"A repair lands on the red layer itself; the hold lifts once its checks pass. "+
		"`saddle sentinel ack` acknowledges it and `saddle unstack <task>` drops the red task", strings.Join(parts, "; "))
}

// ciRedFrozen is the land hook: each file a red layer or a layer it holds
// changed, mapped to "<layer>, ci-red on <red>". Queued work that changes
// one would stack above red CI, so land holds it. Repairs of a red layer are
// never held: they are how it goes green.
func (a *App) ciRedFrozen() (map[string]string, map[string]bool) {
	s, err := a.CIRed()
	if err != nil || len(s.holding()) == 0 {
		return nil, nil
	}
	stack, err := a.landedStack()
	if err != nil {
		return nil, nil
	}
	frozen := map[string]string{}
	exempt := map[string]bool{}
	for _, r := range s.holding() {
		for _, rec := range s.Repairs {
			if rec.Task == r.Task {
				exempt[rec.Repair] = true
			}
		}
		for _, l := range stack {
			if (l.ID != r.Task && !slices.Contains(r.Held, l.ID)) || l.From == "" || l.Lost != "" {
				continue
			}
			files, _ := gitx.ChangedFiles(a.Root, l.From, l.To)
			for _, f := range files {
				if _, ok := frozen[f]; !ok {
					frozen[f] = l.ID + ", ci-red on " + r.Task
				}
			}
		}
	}
	return frozen, exempt
}

// CIRedCovers says why red CI keeps auto-merge off task: it is a red layer
// or above one. Acked holds still cover it; a red PR never merges.
func (a *App) CIRedCovers(task string) string {
	s, err := a.CIRed()
	if err != nil {
		return "can't read the ci-red state: " + err.Error()
	}
	for _, r := range s.Red {
		if r.Task == task {
			return fmt.Sprintf("CI is red on its PR (%s)", strings.Join(r.Checks, ", "))
		}
		if slices.Contains(r.Held, task) {
			return fmt.Sprintf("CI is red on %s below it (%s)", r.Task, strings.Join(r.Checks, ", "))
		}
	}
	return ""
}

// CIRedHold is one red layer and what it holds, for status and the TUI.
type CIRedHold struct {
	Task   string   `json:"task"`
	PR     string   `json:"pr"`
	Head   string   `json:"head"`
	Checks []string `json:"checks"`
	Held   []string `json:"held,omitempty"`   // stacked layers above it
	Queued []string `json:"queued,omitempty"` // queued work land holds for it
	Repair string   `json:"repair,omitempty"` // the repair task working on it
	// Attempts counts the repairs spawned since it was last green.
	Attempts  int  `json:"attempts"`
	Acked     bool `json:"acked,omitempty"`
	Escalated bool `json:"escalated,omitempty"`
}

// CIRedHolds lists the red layers and what each holds back. It reads only
// saddle's state and the store, never GitHub.
func (a *App) CIRedHolds() ([]CIRedHold, error) {
	s, err := a.CIRed()
	if err != nil || len(s.Red) == 0 {
		return nil, err
	}
	frozen, exempt := a.ciRedFrozen()
	entries, err := a.Store.Train()
	if err != nil {
		return nil, err
	}
	var out []CIRedHold
	for _, r := range s.Red {
		h := CIRedHold{Task: r.Task, PR: r.PR, Head: r.Head, Checks: r.Checks, Held: r.Held,
			Repair: r.Repair, Acked: r.Acked, Escalated: r.Escalated}
		for _, rec := range s.Repairs {
			if rec.Task == r.Task {
				h.Attempts++
			}
		}
		if !r.Acked {
			for _, e := range entries {
				if e.State != store.Queued || exempt[e.Task] {
					continue
				}
				if f := a.touches(e.Task, frozen); strings.HasSuffix(strings.TrimSuffix(f, ")"), "ci-red on "+r.Task) {
					h.Queued = append(h.Queued, e.Task)
				}
			}
		}
		out = append(out, h)
	}
	return out, nil
}

// AckCIRed acknowledges every red layer: prs and land stop holding work
// back for them until a layer goes red on a new head. Auto-merge still
// never merges a red PR or one above it.
func (a *App) AckCIRed() ([]CIRedLayer, error) {
	s, err := a.CIRed()
	if err != nil {
		return nil, err
	}
	var acked []CIRedLayer
	for i := range s.Red {
		if !s.Red[i].Acked {
			s.Red[i].Acked = true
			acked = append(acked, s.Red[i])
			a.Store.Event(s.Red[i].Task, "ci_red_ack", strings.Join(s.Red[i].Checks, ", "))
		}
	}
	if len(acked) == 0 {
		return nil, nil
	}
	return acked, a.SetCIRed(s)
}

// CIRedOwns reports whether the ci-red watcher handles task's red CI: it is
// stacked with a PR and its agent is gone, so the watcher holds, repairs
// and reports it. ciwatch defers to it for such tasks.
func (a *App) CIRedOwns(task string) bool {
	t, err := a.Store.Task(task)
	if err != nil || t.Active() {
		return false
	}
	ts, err := a.CIRedTargets()
	return err == nil && slices.ContainsFunc(ts, func(x CIRedTarget) bool { return x.Task == task })
}
