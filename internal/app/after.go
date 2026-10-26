package app

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// After records that task builds on deps once it is spawned (#675): its
// files need not overlap theirs and no issue need be shared, so without the
// edge prs can put a dependency in its own stack on base and task's layer
// fails at its tip. The edges go in the same .saddle/deps file spawn
// writes. It returns the deps that were new.
func (a *App) After(task string, deps []string) ([]string, error) {
	if _, err := a.Store.Task(task); err != nil {
		return nil, fmt.Errorf("after: no task %s", task)
	}
	var want []string
	for _, d := range deps {
		d = strings.TrimSpace(d)
		if d == "" || slices.Contains(want, d) {
			continue
		}
		if d == task {
			return nil, fmt.Errorf("after: %s can't run after itself", task)
		}
		if _, err := a.Store.Task(d); err != nil {
			return nil, fmt.Errorf("after: no task %s", d)
		}
		if a.runsAfter(d, task) {
			return nil, fmt.Errorf("after: %s already runs after %s, so %s can't run after it", d, task, task)
		}
		want = append(want, d)
	}
	if len(want) == 0 {
		return nil, errors.New("after: name at least one task it runs after")
	}
	var added []string
	for _, d := range want {
		ok, err := a.addAfter(task, d, "recorded with saddle after")
		if err != nil {
			return added, err
		}
		if ok {
			added = append(added, d)
		}
	}
	return added, nil
}

// runsAfter reports whether task reaches dep through its after edges.
func (a *App) runsAfter(task, dep string) bool {
	seen := map[string]bool{}
	next := []string{task}
	for len(next) > 0 {
		id := next[len(next)-1]
		next = next[:len(next)-1]
		for _, d := range a.TaskAfter(id) {
			if d == dep {
				return true
			}
			if !seen[d] {
				seen[d] = true
				next = append(next, d)
			}
		}
	}
	return false
}

// afterHint explains a red gate layer by a probably missing after edge: a
// sibling not below task in the stack adds what out says is missing. It
// names the command that records the edge and never records it itself, so
// a wrong guess costs a line of output, not a reshaped stack (#675).
func (a *App) afterHint(task, out string) string {
	sib, thing, ok := a.dependencyExplains(task, out)
	if !ok || slices.Contains(a.TaskAfter(task), sib) {
		return ""
	}
	return fmt.Sprintf("%s adds %s, which this check says is missing, but %s isn't recorded as running after %s. If it builds on that work, run `saddle after %s %s` and prs again.",
		sib, thing, task, sib, task, sib)
}
