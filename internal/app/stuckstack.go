package app

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// The stuck-stack alarm (#223). A red or conflicting layer that nothing is
// fixing blocks every layer above it, and holds are routine, so nobody hears
// about it. Once a stack has stayed that way for [train] stuck_after with no
// task working on it, the orchestrator is interrupted once, naming the stack,
// the layer and why. It is not told again for the same stack and reason
// until that clears.

// Stuck reasons.
const (
	StuckCIRed      = "ci-red"     // its PR's checks failed (cired.go)
	StuckPrepublish = "prepublish" // its own tip fails the pre-publish gate
	StuckAtRisk     = "at-risk"    // the stack sentinel flagged it: a conflict or drift
)

// StuckAlarm is one stack the alarm fired for.
type StuckAlarm struct {
	Stack  string        `json:"stack"` // the stack's bottom layer
	Layer  string        `json:"layer"`
	Reason string        `json:"reason"`
	Detail string        `json:"detail"`
	For    time.Duration `json:"for"`
}

// stuckLayer is a red or conflicting layer seen now.
type stuckLayer struct {
	Layer, Reason, Detail string
	Since                 time.Time // when it went bad, if known
	Fixing                bool      // a live task is working on it
}

// stuckState is what the alarm remembers, saved under .saddle.
type stuckState struct {
	// Seen is when each layer|reason was first seen bad with nobody fixing it.
	Seen map[string]time.Time `json:"seen,omitempty"`
	// Alerted maps each stack|reason the orchestrator was told about to the
	// layers it was told for.
	Alerted map[string][]string `json:"alerted,omitempty"`
}

func (a *App) stuckPath() string { return a.stateDir("stuck-stack.json") }

func (a *App) stuckState() stuckState {
	var s stuckState
	if b, err := os.ReadFile(a.stuckPath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	if s.Seen == nil {
		s.Seen = map[string]time.Time{}
	}
	if s.Alerted == nil {
		s.Alerted = map[string][]string{}
	}
	return s
}

func (a *App) setStuckState(s stuckState) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.stuckPath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.stuckPath())
}

// stuckLayers lists the layers that are red or conflicting now.
func (a *App) stuckLayers() ([]stuckLayer, error) {
	active := func(id string) bool {
		t, err := a.Store.Task(id)
		return err == nil && t.Active()
	}
	var out []stuckLayer
	cr, err := a.CIRed()
	if err != nil {
		return nil, err
	}
	for _, l := range cr.Red {
		if l.Acked {
			continue // someone acknowledged it
		}
		out = append(out, stuckLayer{Layer: l.Task, Reason: StuckCIRed, Since: l.Since,
			Detail: "CI is red on its PR " + l.PR + " (" + strings.Join(l.Checks, ", ") + ")",
			Fixing: l.Repair != "" && active(l.Repair) || active(l.Task)})
	}
	g, err := a.Gate()
	if err != nil {
		return nil, err
	}
	for _, r := range g.Red {
		out = append(out, stuckLayer{Layer: r.Task, Reason: StuckPrepublish, Since: r.Since,
			Detail: fmt.Sprintf("its own tip %s fails its %s check (`%s`)", short(r.Head), r.Check.Name, r.Check.Cmd),
			Fixing: active(r.Task)})
	}
	f, flagged, err := a.Flag()
	if err != nil {
		return nil, err
	}
	if flagged && !f.Acked {
		out = append(out, stuckLayer{Layer: f.Task, Reason: StuckAtRisk, Detail: f.Cause, Fixing: active(f.Task)})
	}
	return out, nil
}

// CheckStuck runs the stuck-stack alarm at now: it interrupts the
// orchestrator once for each stack and reason that has been red or
// conflicting for [train] stuck_after with no task fixing it. The sentinel
// calls it every cycle. A cycle that would alert while the train is busy
// waits for the next one, since naming the stack reads the PR layout.
func (a *App) CheckStuck(now time.Time) ([]StuckAlarm, error) {
	after := a.Cfg.Train.StuckAfter
	if after <= 0 {
		after = 30 * time.Minute
	}
	layers, err := a.stuckLayers()
	if err != nil {
		return nil, err
	}
	s := a.stuckState()
	seen := map[string]time.Time{}
	var due []stuckLayer
	for _, l := range layers {
		key := l.Layer + "|" + l.Reason
		if l.Fixing {
			continue // its clock starts when nobody is on it
		}
		since, ok := s.Seen[key]
		if !ok {
			since = now
			if !l.Since.IsZero() && l.Since.Before(now) {
				since = l.Since
			}
		}
		seen[key] = since
		if now.Sub(since) >= after {
			due = append(due, l)
		}
	}
	s.Seen = seen
	var alarms []StuckAlarm
	if len(due) > 0 {
		alarms, err = a.alarmStuck(due, seen, now, &s)
		if err != nil {
			return nil, err
		}
	}
	// Once none of its layers is bad for that reason, a stack may alert again.
	for k, ls := range s.Alerted {
		_, reason, _ := strings.Cut(k, "|")
		if !slices.ContainsFunc(layers, func(l stuckLayer) bool { return l.Reason == reason && slices.Contains(ls, l.Layer) }) {
			delete(s.Alerted, k)
		}
	}
	return alarms, a.setStuckState(s)
}

// alarmStuck tells the orchestrator about each due layer whose stack and
// reason it hasn't heard about yet.
func (a *App) alarmStuck(due []stuckLayer, seen map[string]time.Time, now time.Time, s *stuckState) ([]StuckAlarm, error) {
	unlock, ok, err := a.TryLockTrain()
	if err != nil || !ok {
		return nil, err
	}
	stackOf, err := a.stackNames()
	unlock()
	if err != nil {
		return nil, err
	}
	var out []StuckAlarm
	for _, l := range due {
		stack := stackOf[l.Layer]
		if stack == "" {
			stack = l.Layer // out of the PR stack: it stands for itself
		}
		key := stack + "|" + l.Reason
		if _, told := s.Alerted[key]; told {
			if !slices.Contains(s.Alerted[key], l.Layer) {
				s.Alerted[key] = append(s.Alerted[key], l.Layer)
			}
			continue
		}
		al := StuckAlarm{Stack: stack, Layer: l.Layer, Reason: l.Reason, Detail: l.Detail, For: now.Sub(seen[l.Layer+"|"+l.Reason]).Round(time.Minute)}
		if err := a.Notify(OrchestratorID, store.NoticeAction, stuckNotice(al)); err != nil {
			return out, err
		}
		a.Store.Event(l.Layer, "stack_stuck", key)
		s.Alerted[key] = []string{l.Layer}
		out = append(out, al)
	}
	return out, nil
}

// stackNames maps each stacked task to its stack's bottom layer.
func (a *App) stackNames() (map[string]string, error) {
	stack, err := a.landedStack()
	if err != nil || len(stack) == 0 {
		return nil, err
	}
	layout, _, err := a.stackLayout(stack)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for i, l := range stack {
		g := layout[i].Group
		if g < 0 || g >= len(stack) {
			g = i
		}
		out[l.ID] = stack[g].ID
	}
	return out, nil
}

func stuckNotice(al StuckAlarm) string {
	what := map[string]string{StuckCIRed: "red", StuckPrepublish: "red at its own tip", StuckAtRisk: "at risk"}[al.Reason]
	return fmt.Sprintf("Stuck stack: the stack from %s has been %s at layer %s for %s and no task is fixing it: %s. "+
		"Everything above %s is held until it's fixed; this needs you: spawn a fix for %s, restack, or `saddle unstack %s`.",
		al.Stack, what, al.Layer, al.For, al.Detail, al.Layer, al.Layer, al.Layer)
}
