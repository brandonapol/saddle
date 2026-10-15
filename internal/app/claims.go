package app

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/brandonapol/saddle/internal/claims"
	"github.com/brandonapol/saddle/internal/store"
)

// ClaimResult reports what a claim request got.
type ClaimResult struct {
	Granted []string          `json:"granted"`
	Denied  map[string]string `json:"denied,omitempty"`
}

// Claim grants every requested glob that no other active task overlaps.
func (a *App) Claim(task string, want []string) (ClaimResult, error) {
	res := ClaimResult{Denied: map[string]string{}}
	for i, w := range want {
		want[i] = claims.Clean(w)
	}
	granted, err := a.Store.Claim(task, func(all map[string][]string) ([]string, error) {
		c := claims.Conflicts(all, task, want)
		var g []string
		for _, w := range want {
			if owner, bad := c[w]; bad {
				res.Denied[w] = owner
			} else {
				g = append(g, w)
			}
		}
		return g, nil
	})
	res.Granted = granted
	if len(granted) > 0 {
		a.Store.Event(task, "claim", strings.Join(granted, ","))
	}
	return res, err
}

// Decision is the verdict on a write by an agent.
type Decision struct {
	Allow  bool
	Reason string
	// Owner is the task whose path claim denied the write. Serial-file and
	// outside-worktree denies leave it empty, so a symbol rule can tell a
	// claim deny (which it may override) from those (which it never does).
	Owner string
}

// CheckWrite decides whether task may write the file at abs. Paths nobody owns
// are claimed for the task on first write, so claims track real work.
func (a *App) CheckWrite(task, abs string) Decision {
	t, err := a.Store.Task(task)
	if err != nil || t.Role != store.RoleWorker || !t.Active() {
		return Decision{Allow: true}
	}
	abs = filepath.Clean(abs)
	wt := filepath.Clean(t.Worktree)
	rel, err := filepath.Rel(wt, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		if r, err := filepath.Rel(a.Root, abs); err == nil && !strings.HasPrefix(r, "..") {
			return Decision{Reason: fmt.Sprintf("%s is outside your worktree. You work in %s on branch %s; edit your copy of the file there instead.", abs, wt, t.Branch)}
		}
		return Decision{Allow: true} // outside the repo entirely (scratch files etc.)
	}
	rel = filepath.ToSlash(rel)
	if strings.HasPrefix(rel, ".git/") {
		return Decision{Allow: true}
	}
	for _, g := range a.Cfg.Serial {
		if claims.Match(g, rel) {
			mine, _ := a.Store.Claims()
			if _, ok := claims.Covering(mine[task], rel); ok {
				return Decision{Allow: true}
			}
			return Decision{Reason: fmt.Sprintf("%s is a serial file (%s) owned by the merge train. Don't edit it; it is regenerated when your branch lands. If you truly need a change, say so in your done summary.", rel, g)}
		}
	}
	var reason, owner string
	_, err = a.Store.Claim(task, func(all map[string][]string) ([]string, error) {
		if _, ok := claims.Covering(all[task], rel); ok {
			return nil, nil
		}
		var glob string
		if owner, glob = claims.Owner(all, task, rel); owner != "" {
			reason = a.denyReason(rel, owner, glob)
			return nil, nil
		}
		return []string{rel}, nil
	})
	if err != nil {
		return Decision{Allow: true} // fail open
	}
	if reason != "" && a.ciRepairMayWrite(task, rel, owner) {
		a.Store.Event(task, "ci_repair_carveout", rel)
		return Decision{Allow: true}
	}
	if reason != "" {
		a.Store.Event(task, "deny", rel)
		return Decision{Reason: reason, Owner: owner}
	}
	return Decision{Allow: true}
}

// denyReason tells a task why it may not write rel, which owner's claim
// covers it, and how to get it without a hand fix (#292).
func (a *App) denyReason(rel, owner, glob string) string {
	ot, _ := a.Store.Task(owner)
	return fmt.Sprintf("%s is claimed by %s (%q, claim %s). Don't edit it: work around it (depend on an interface, stub it in tests), or finish your part and mention the needed change in your done summary. "+
		"If you truly need this file, free it: ask %s (message tool) or the orchestrator to release just it (release tool with task=%s paths=[%q], or `saddle release --task %s %s`); a release under a glob takes effect at once and the rest of %s stays %s's.",
		rel, owner, ot.Title, glob, owner, owner, rel, owner, rel, glob, owner)
}

// ciRepairMayWrite reports whether task is a live CI repair that may write
// rel over owner's claim. The files its red layer landed already merged, so
// a running task's claim over them must not block the repair (#292). A done
// task's claims only guard its queued work, which red CI holds behind the
// repair anyway, so they block it nowhere (#466): the train rebases what
// overlaps when the hold lifts.
func (a *App) ciRepairMayWrite(task, rel, owner string) bool {
	s, err := a.CIRed()
	if err != nil {
		return false
	}
	for _, red := range a.ciRepairing(s, task) {
		if o, err := a.Store.Task(owner); err == nil && o.Status == store.Done {
			return true
		}
		if slices.Contains(a.layerFiles(red), rel) {
			return true
		}
	}
	return false
}

// ciRepairing lists the red layers task is the live CI repair of. A repair
// is recorded only after Spawn returns, and its agent may write before
// that (#568): until a record names it, a live task titled as a red layer's
// repair counts as that layer's.
func (a *App) ciRepairing(s CIRedState, task string) []string {
	var out []string
	recorded := false
	for _, r := range s.Repairs {
		if r.Repair != task {
			continue
		}
		recorded = true
		if r.State == ciRepairSpawned {
			out = append(out, r.Task)
		}
	}
	if recorded {
		return out
	}
	t, err := a.Store.Task(task)
	if err != nil || !t.Active() {
		return nil
	}
	if orig, ok := strings.CutPrefix(t.Title, ciFixPrefix); ok {
		orig, _, _ = strings.Cut(orig, " ")
		if slices.ContainsFunc(s.Red, func(l CIRedLayer) bool { return l.Task == orig }) {
			return []string{orig}
		}
	}
	return nil
}
