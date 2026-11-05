package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// Gate-red repairs (#773). A layer red at the pre-publish gate is never
// pushed, so CI never goes red on it and CIRedRepair never sees it, and its
// agent is gone, so its head never changes: it held its stack until a human
// acted. Now a gate red whose agent is gone is a pending repair of kind
// "gate" (see repairpending.go), and RetryRepairs spawns it:
//   - first it runs the failing check on base alone, once per base commit.
//     Base red too means the environment or base is broken (#726, #727), not
//     the layer: no repair, one base-red event and notice. When base moves
//     it is checked again.
//   - if the layer's PR head moved since the gate saw it red (a restack onto
//     a base that may hold the fix), that head is checked first, and a green
//     one clears the red record instead.
//   - the repair is a standard-difficulty fix on the layer, seeded with the
//     gate's output tail. It is recorded as a CI repair too, so when it lands
//     FoldCIRepairs folds its commits into the red layer, as for CI reds.
//   - one repair at a time, and per red head; [ci] repair_attempts caps them
//     as for CI, and the orchestrator is told once the cap is spent. A repair
//     the owner killed is a decline: no more are spawned.

// Gate repair events, on the red task.
const (
	eventGateRepairSpawned = "prepublish_repair_spawned" // data: repair task id
	eventGateRepairFailed  = "prepublish_repair_failed"  // data: why it couldn't be spawned
	eventGateBaseRed       = "prepublish_base_red"       // data: base commit, check and its tail
	eventGateRepairCap     = "prepublish_repair_escalated"
)

// Base check outcomes, by "<base commit> <cmd>".
const (
	gateBaseGreen = "green"
	gateBaseRed   = "red"
	gateBaseEnv   = "env" // the check failed on the environment on base
)

// gateRepairRecord is one gate repair of a red layer: spawned (Repair set)
// or pending (Repair empty, Note says why its last spawn failed).
type gateRepairRecord struct {
	Task   string   `json:"task"`
	Head   string   `json:"head"`
	Repair string   `json:"repair,omitempty"`
	Note   string   `json:"note,omitempty"`
	Claims []string `json:"claims,omitempty"`
}

// gateRepairState is what saddle knows of gate repairs, saved under .saddle.
type gateRepairState struct {
	Repairs []gateRepairRecord `json:"repairs,omitempty"`
	// Bases caches the failing checks run on base alone.
	Bases map[string]string `json:"bases,omitempty"`
	// Escalated are the "task@head" whose repair cap was reported.
	Escalated []string `json:"escalated,omitempty"`
}

func (a *App) gateRepairPath() string { return a.stateDir("prepublish-repair.json") }

func (a *App) gateRepairs() (gateRepairState, error) {
	var s gateRepairState
	b, err := os.ReadFile(a.gateRepairPath())
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("%s: %w", a.gateRepairPath(), err)
	}
	return s, nil
}

func (a *App) setGateRepairs(s gateRepairState) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.gateRepairPath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.gateRepairPath())
}

// gateBase is the commit the gate's base check runs on: origin's base
// branch, else the local one; "" when neither resolves.
func (a *App) gateBase() string {
	for _, ref := range []string{"refs/remotes/origin/" + a.Cfg.Base, a.Cfg.Base} {
		if sha, err := gitx.RevParse(a.Root, ref); err == nil {
			return sha
		}
	}
	return ""
}

func gateBaseKey(base string, c GateCheck) string { return base + " " + c.Cmd }

// gateRepairStatus says what stops a gate repair of r now: "live" (one is
// working, or landed on this head), "declined" (the owner killed one) or
// "cap" (the attempts are spent); "" when nothing does. It also returns
// the attempts so far.
func (a *App) gateRepairStatus(s gateRepairState, r GateRed) (string, int) {
	n := 0
	for _, rec := range s.Repairs {
		if rec.Task != r.Task || rec.Repair == "" {
			continue
		}
		t, err := a.Store.Task(rec.Repair)
		if err != nil {
			continue
		}
		if !ciRepairDead(t) {
			if rec.Head == r.Head || t.Status != store.Landed {
				return "live", n
			}
		} else if t.Status == store.Killed {
			if !a.internalKill(t.ID) {
				return "declined", n
			}
			continue // saddle killed it; it didn't get the chance (#772)
		}
		n++
	}
	if n >= a.Cfg.CI.RepairAttempts {
		return "cap", n
	}
	return "", n
}

// gatePendingRepairs are the gate-red layers whose agent is gone and whose
// repair saddle owes: none live, base not known red at the failing check,
// not declined, and the cap not yet reported. It never spawns or writes.
func (a *App) gatePendingRepairs() []PendingRepair {
	g, err := a.Gate()
	if err != nil || len(g.Red) == 0 {
		return nil
	}
	s, err := a.gateRepairs()
	if err != nil {
		return nil
	}
	base := a.gateBase()
	var out []PendingRepair
	for _, r := range g.Red {
		owner, err := a.Store.Task(r.Task)
		if err != nil || owner.Active() {
			continue // its live agent fixes it
		}
		if v, ok := s.Bases[gateBaseKey(base, r.Check)]; ok && v != gateBaseGreen {
			continue // base is red too: not the layer's to fix
		}
		why, _ := a.gateRepairStatus(s, r)
		if why == "live" || why == "declined" || why == "cap" && slices.Contains(s.Escalated, r.Task+"@"+r.Head) {
			continue
		}
		p := PendingRepair{Kind: "gate", Task: r.Task}
		if i := slices.IndexFunc(s.Repairs, func(x gateRepairRecord) bool { return x.Task == r.Task && x.Repair == "" }); i >= 0 {
			p.Claims, p.Err = s.Repairs[i].Claims, s.Repairs[i].Note
		} else {
			p.Claims = a.layerFiles(r.Task)
		}
		out = append(out, p)
	}
	return out
}

// GateRepairPending reports whether task's gate repair is pending.
func (a *App) GateRepairPending(task string) bool {
	return slices.ContainsFunc(a.gatePendingRepairs(), func(p PendingRepair) bool { return p.Task == task })
}

// retryGateRepair decides what to do about p's gate red and does it: check
// base, re-check a moved head, escalate a spent cap, else spawn the repair.
func (a *App) retryGateRepair(p PendingRepair) (string, error) {
	g, err := a.Gate()
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(g.Red, func(r GateRed) bool { return r.Task == p.Task })
	if i < 0 {
		return "", nil
	}
	r := g.Red[i]
	owner, err := a.Store.Task(r.Task)
	if err != nil || owner.Active() {
		return "", err
	}
	s, err := a.gateRepairs()
	if err != nil {
		return "", err
	}
	base := a.gateBase()
	a.pruneGateRepairs(&s, g, base)

	why, n := a.gateRepairStatus(s, r)
	switch why {
	case "live", "declined":
		return "", a.setGateRepairs(s)
	case "cap":
		key := r.Task + "@" + r.Head
		if !slices.Contains(s.Escalated, key) {
			s.Escalated = append(s.Escalated, key)
			a.Store.Event(r.Task, eventGateRepairCap, fmt.Sprintf("%d repairs", n))
			_ = a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
				"%s is still red at the pre-publish gate (%s check `%s`) after %d repairs, so saddle spawns no more. "+
					"It holds %s and the layers above it. Fix it by hand, or `saddle unstack %s` to drop it.",
				r.Task, r.Check.Name, r.Check.Cmd, n, r.Task, r.Task))
		}
		return "", a.setGateRepairs(s)
	}

	if base != "" {
		key := gateBaseKey(base, r.Check)
		if _, ok := s.Bases[key]; !ok {
			v, out, err := a.gateCheckBase(base, r.Check)
			if err != nil {
				return "", err
			}
			if s.Bases == nil {
				s.Bases = map[string]string{}
			}
			s.Bases[key] = v
			if v != gateBaseGreen {
				a.Store.Event(r.Task, eventGateBaseRed, fmt.Sprintf("%s %s: %s", short(base), r.Check.Name, tail(out, 3)))
				what := "red on base"
				if v == gateBaseEnv {
					what = "red on base, on the environment,"
				}
				_ = a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
					"%s is red at the pre-publish gate, but its %s check (`%s`) is %s too (%s %s), so the layer isn't to blame "+
						"and saddle spawns no repair for it (#773). Fix %s or the gate's environment; saddle checks again when %s moves.\nLast lines on base:\n%s",
					r.Task, r.Check.Name, r.Check.Cmd, what, a.Cfg.Base, short(base), a.Cfg.Base, a.Cfg.Base, tail(out, 5)))
			}
		}
		if s.Bases[key] != gateBaseGreen {
			return "", a.setGateRepairs(s)
		}
	}

	if moved, green := a.gateRecheckMoved(r); moved {
		if green {
			return "", a.setGateRepairs(s) // the moved head passes: the record is gone
		}
		if g, err := a.Gate(); err == nil {
			if j := slices.IndexFunc(g.Red, func(x GateRed) bool { return x.Task == r.Task }); j >= 0 {
				r = g.Red[j]
			}
		}
	}

	files := a.layerFiles(r.Task)
	req := SpawnReq{
		Title:      gateFixTitle(owner),
		Prompt:     a.gateRepairPrompt(owner, r, n+1),
		ModelHint:  "sonnet", // saddle's default for a repair, not a pin (#505)
		Difficulty: config.DifficultyStandard,
		Parent:     OrchestratorID,
		Issue:      owner.Issue,
		Claims:     slices.Clone(files),
		For:        r.Task, // its own pending repair reserves these files
		Confirm:    true,
	}
	t, err := a.Spawn(req)
	if err != nil && len(req.Claims) > 0 {
		req.Claims = nil // live work may hold the layer's files; the repair claims as it goes
		t, err = a.Spawn(req)
	}
	s.Repairs = slices.DeleteFunc(s.Repairs, func(x gateRepairRecord) bool { return x.Task == r.Task && x.Repair == "" })
	if err != nil {
		why := err.Error()
		s.Repairs = append(s.Repairs, gateRepairRecord{Task: r.Task, Head: r.Head, Note: why, Claims: files})
		if last, ok := a.lastEvent(r.Task, eventGateRepairFailed); !ok || last != why {
			a.Store.Event(r.Task, eventGateRepairFailed, why)
			_ = a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
				"%s is red at the pre-publish gate and saddle couldn't spawn its repair: %s\n"+
					"The repair is pending: saddle retries it every cycle, and it takes the next free slot before new tickets. "+
					"Spawn one titled %q yourself if that keeps failing.", r.Task, why, req.Title))
		}
		return "", a.setGateRepairs(s)
	}
	s.Repairs = append(s.Repairs, gateRepairRecord{Task: r.Task, Head: r.Head, Repair: t.ID})
	a.Store.Event(r.Task, eventGateRepairSpawned, t.ID)
	// Recorded as a CI repair too, so FoldCIRepairs folds it into the layer.
	ci, err := a.CIRed()
	if err == nil {
		ci.Repairs = append(ci.Repairs, CIRepairRecord{Task: r.Task, Head: r.Head, Repair: t.ID, State: ciRepairSpawned})
		err = a.SetCIRed(ci)
	}
	return t.ID, errors.Join(err, a.setGateRepairs(s), a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf(
		"%s is red at the pre-publish gate (%s check), so saddle spawned %s (attempt %d of %d) to fix it on its layer; its commits join %s's layer when it lands.",
		r.Task, r.Check.Name, t.ID, n+1, a.Cfg.CI.RepairAttempts, r.Task)))
}

// pruneGateRepairs drops what no longer matters: records of layers no
// longer red whose repair is done with, and base checks of an old base.
func (a *App) pruneGateRepairs(s *gateRepairState, g GateState, base string) {
	red := func(task string) bool { return slices.ContainsFunc(g.Red, func(r GateRed) bool { return r.Task == task }) }
	s.Repairs = slices.DeleteFunc(s.Repairs, func(rec gateRepairRecord) bool {
		if red(rec.Task) {
			return false
		}
		if rec.Repair == "" {
			return true
		}
		t, err := a.Store.Task(rec.Repair)
		return err != nil || !t.Active()
	})
	s.Escalated = slices.DeleteFunc(s.Escalated, func(k string) bool {
		task, _, _ := strings.Cut(k, "@")
		return !red(task)
	})
	for k := range s.Bases {
		if !strings.HasPrefix(k, base+" ") {
			delete(s.Bases, k)
		}
	}
}

// gateCheckBase runs check on base alone.
func (a *App) gateCheckBase(base string, c GateCheck) (string, string, error) {
	red, err := a.runGate([]gateJob{{task: "", head: base}}, []GateCheck{c}, nil)
	if err != nil {
		return "", "", err
	}
	r, ok := red[0]
	switch {
	case !ok:
		return gateBaseGreen, "", nil
	case r.env != nil:
		return gateBaseEnv, r.out, nil
	}
	return gateBaseRed, r.out, nil
}

// gateRecheckMoved checks r's layer at its current PR head when that moved
// since the gate saw it red, and records what it found. moved is false when
// the head is unchanged or can't be told; green says the new head passed.
func (a *App) gateRecheckMoved(r GateRed) (moved, green bool) {
	stack, err := a.landedStack()
	if err != nil {
		return false, false
	}
	layout, _, err := a.stackLayout(stack)
	if err != nil {
		return false, false
	}
	i := slices.IndexFunc(stack, func(l landedTask) bool { return l.ID == r.Task })
	if i < 0 || i >= len(layout) || layout[i].Head == "" || layout[i].Head == r.Head {
		return false, false
	}
	head := layout[i].Head
	red, err := a.runGate([]gateJob{{i: i, task: r.Task, head: head}}, []GateCheck{r.Check}, nil)
	if err != nil {
		return false, false
	}
	res, failed := red[i]
	if failed && res.env != nil {
		return false, false // proved nothing (#274)
	}
	var reds []GateRed
	if failed {
		g := r
		g.Head, g.Tail, g.TimedOut, g.Directory = head, tail(res.out, gateTailLines), res.timedOut, res.dir
		reds = append(reds, g)
	}
	if err := a.recordGate(reds, map[string]bool{r.Task: true}, r.Source); err != nil {
		return false, false
	}
	return true, !failed
}

func gateFixTitle(owner store.Task) string {
	return "Fix pre-publish gate for " + owner.ID + " " + owner.Title
}

// gateRepairPrompt tells the repair what is red at the gate and that its
// fix joins the red layer.
func (a *App) gateRepairPrompt(owner store.Task, r GateRed, attempt int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %q has landed and its agent is gone, but its layer is red at saddle's pre-publish gate, so it was never pushed, "+
		"and the layers above it are held: %s. You fix it.\n\n", owner.ID, owner.Title, strings.Join(r.Held, ", "))
	fmt.Fprintf(&b, "At its PR head %s, the %s check `%s` fails", short(r.Head), r.Check.Name, r.Check.Cmd)
	if r.TimedOut {
		b.WriteString(" (it timed out)")
	}
	b.WriteString(". The check passes on base alone, so the layer's own commits cause it.\n")
	if r.Tail != "" {
		b.WriteString("Last lines of its output:\n" + r.Tail + "\n")
	}
	if r.Hint != "" {
		b.WriteString("Hint: " + r.Hint + "\n")
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "Your branch is cut from %s, so it has %s's work (branch %s). Reproduce the failure with `%s`, fix it with the smallest change, "+
		"run the tests, commit, and call the saddle done tool. When your branch lands, saddle folds your commits into %s's layer, "+
		"and the gate checks it again. Fix only what makes %s red at the gate. Don't touch %s yourself and don't open a PR. "+
		"This is repair attempt %d of %d.",
		a.Cfg.Integration, owner.ID, owner.Branch, r.Check.Cmd, owner.ID, owner.ID, owner.Branch, attempt, a.Cfg.CI.RepairAttempts)
	return b.String()
}
