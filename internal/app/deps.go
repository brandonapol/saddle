package app

import (
	"fmt"
	"os"
	"slices"
	"strings"
)

// EventDependsOn is the events-table kind recording a task's explicit after
// edges, comma-separated, when it is spawned.
const EventDependsOn = "depends_on"

// A task spawned with after builds on those tasks' unmerged work (#193):
// a CI job that runs a make target another task adds, say. Their files need
// not overlap, so clustering would otherwise publish them as unrelated PRs on
// base and the dependent one fails CI until the other merges. The edges live
// in .saddle/deps/<task>, one id per line, written once at spawn.

func (a *App) depsPath(id string) string { return a.stateDir("deps", id) }

// cleanAfter trims and dedupes after and checks every id names another task.
func (a *App) cleanAfter(id string, after []string) ([]string, error) {
	var out []string
	for _, d := range after {
		d = strings.TrimSpace(d)
		if d == "" || slices.Contains(out, d) {
			continue
		}
		if d == id {
			return nil, fmt.Errorf("spawn: after: %s can't run after itself", id)
		}
		if _, err := a.Store.Task(d); err != nil {
			return nil, fmt.Errorf("spawn: after: no task %s", d)
		}
		out = append(out, d)
	}
	return out, nil
}

// setAfter records id's after edges and logs them.
func (a *App) setAfter(id string, after []string) error {
	if len(after) == 0 {
		return nil
	}
	if err := os.MkdirAll(a.stateDir("deps"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(a.depsPath(id), []byte(strings.Join(after, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	a.Store.Event(id, EventDependsOn, strings.Join(after, ","))
	return nil
}

// TaskAfter returns the tasks id was spawned to run after, or nil.
func (a *App) TaskAfter(id string) []string {
	b, err := os.ReadFile(a.depsPath(id))
	if err != nil {
		return nil
	}
	return strings.Fields(string(b))
}
