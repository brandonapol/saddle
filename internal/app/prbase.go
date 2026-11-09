package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"

	"github.com/brandonapol/saddle/internal/store"
)

// Which branch a PR targets (#358, #359). The layout stacks a task on the
// earlier tasks it depends on (see clusterLayers). The only after edges are
// the ones a spawner declared: saddle no longer infers one from a task's
// text or from a hand retarget (ADR 0012 rule 8, #934).
//
// Saddle changes a PR's base only out loud: when the base it sets differs
// from the one GitHub has, it logs a pr_base event naming both, and tells the
// orchestrator when the old one was set by hand. And a layer whose
// dependency has an open PR outside its stack (in the merge queue, say)
// stays where it is until that PR merges, instead of moving onto base.

// EventPRBase is the events-table kind for a PR base saddle changed.
const EventPRBase = "pr_base"

// EventDependencyHold is the events-table kind for a layer prs or restack
// left alone because a task it depends on hasn't merged.
const EventDependencyHold = "dependency_hold"

// EventDependencyDropped is recorded on a layer when saddle stops waiting on
// a dependency it no longer intends to merge (#774); data: the dependency
// and why.
const EventDependencyDropped = "dep_dropped"

func (a *App) prBasesPath() string { return a.stateDir("pr-bases.json") }

// savedBases maps each task to the base saddle last set on its PR.
func (a *App) savedBases() map[string]string {
	m := map[string]string{}
	if b, err := os.ReadFile(a.prBasesPath()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func (a *App) saveBase(task, base string) error {
	m := a.savedBases()
	if m[task] == base {
		return nil
	}
	m[task] = base
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.prBasesPath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.prBasesPath())
}

// taskOnBranch names the saddle task whose branch is branch.
func (a *App) taskOnBranch(branch string) (string, bool) {
	ts, err := a.Store.Tasks()
	if err != nil {
		return "", false
	}
	for _, t := range ts {
		if t.Branch == branch {
			return t.ID, true
		}
	}
	return "", false
}

// setPRBase points t's PR at base and remembers that saddle set it. live is
// the base GitHub reported before ("" when unknown). A change from a base
// saddle didn't set is someone's retarget being undone: the orchestrator
// hears which, and why. A base that is already right isn't edited, and one
// GitHub refuses because the PR is linked into a gh stack is left to the
// relink that follows (#341).
func (a *App) setPRBase(t store.Task, live, base, source string) error {
	if live != base {
		if err := a.Forge().SetPRBase(context.Background(), t.PR, base); err != nil {
			if !linkedBaseErr(err) {
				return err
			}
			a.Store.Event(t.ID, EventPRBaseLinked, fmt.Sprintf("%s: %s → %s refused, the PR is part of a stack (%s)", t.PR, live, base, source))
			return nil
		}
	}
	return a.notePRBase(t, live, base, source)
}

// notePRBase records that saddle set t's PR base to base, logging a change
// from live.
func (a *App) notePRBase(t store.Task, live, base, source string) error {
	set := a.savedBases()[t.ID]
	if err := a.saveBase(t.ID, base); err != nil {
		return err
	}
	if live == "" || live == base {
		return nil
	}
	a.Store.Event(t.ID, EventPRBase, fmt.Sprintf("%s: %s → %s (%s)", t.PR, live, base, source))
	if set == "" || live == set {
		return nil
	}
	return a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
		"%s changed %s's PR %s base from %s back to %s. Someone set %s outside saddle, but saddle stacks a PR only on a stacked task that landed before it. "+
			"To keep tasks together, group them with `saddle stack create <name> <tasks…>`, or open the PR on its own with `saddle publish`.",
		source, t.ID, t.PR, live, base, live))
}

// waitingOn names a task that layer l depends on, outside stack, whose PR is
// still open: l's work isn't complete on base until that PR merges, so
// prs and restack leave l's PR where it is (#358). lookup asks GitHub.
//
// It waits only on a dependency saddle still means to merge (#774). One
// that left the stack another way (unstacked, killed, repairing) keeps its
// PR open but usually never merges it, so the edge is dropped, logged once,
// and l stacks on base; if its commits don't replay there, that is an
// ordinary restack repair.
func (a *App) waitingOn(l landedTask, stack []landedTask, lookup PRLookup) (dep, pr string) {
	var routed []StallRoute
	for _, d := range a.TaskAfter(l.ID) {
		if slices.ContainsFunc(stack, func(o landedTask) bool { return o.ID == d }) {
			continue // the layout stacks on it
		}
		t, err := a.Store.Task(d)
		if err != nil || t.PR == "" {
			continue
		}
		if routed == nil {
			routed, _ = a.StallHeld()
		}
		if why := a.abandonedDep(t, routed); why != "" {
			if last, ok := a.lastEvent(l.ID, EventDependencyDropped); !ok || last != d+": "+why {
				a.Store.Event(l.ID, EventDependencyDropped, d+": "+why)
			}
			continue
		}
		if info, err := lookup(t.PR); err == nil && info.State == "OPEN" {
			return d, t.PR
		}
	}
	return "", ""
}

// abandonedDep is why saddle no longer means to merge dependency t's PR, ""
// when it still does: t is queued, landed in the stack, or a route-around's
// standalone PR.
func (a *App) abandonedDep(t store.Task, routed []StallRoute) string {
	if slices.ContainsFunc(routed, func(r StallRoute) bool { return r.Red == t.ID && r.PR != "" }) {
		return ""
	}
	if t.Status == store.Killed {
		return "it was killed"
	}
	e, ok := a.trainEntry(t.ID)
	if !ok {
		return ""
	}
	switch e.State {
	case TrainSuperseded:
		return "it left the stack as superseded"
	case TrainRepairing:
		return "it is out of the stack for a repair"
	}
	return ""
}

// holdForDependency logs a layer left alone for its dependency and tells
// the orchestrator, once per hold.
func (a *App) holdForDependency(l landedTask, dep, pr, source string) {
	a.Store.Event(l.ID, EventDependencyHold, fmt.Sprintf("%s: %s depends on %s, whose PR %s is still open", source, l.ID, dep, pr))
	p := a.stateDir("deps", l.ID+".held")
	if b, err := os.ReadFile(p); err == nil && string(b) == pr {
		return
	}
	_ = os.WriteFile(p, []byte(pr), 0o644)
	_ = a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf(
		"%s left %s's PR as it was: %s depends on %s, whose PR %s hasn't merged and isn't below it in the stack. "+
			"Once %s merges, the next restack moves %s onto %s.",
		source, l.ID, l.ID, dep, pr, pr, l.ID, a.Cfg.Base))
}

// releaseDependencyHold forgets l's hold once it is published again.
func (a *App) releaseDependencyHold(id string) { _ = os.Remove(a.stateDir("deps", id+".held")) }
