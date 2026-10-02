package app

import (
	"fmt"

	"github.com/brandonapol/saddle/internal/store"
)

// Default spawn caps; [spawn] in config overrides them (#142).
const (
	// DefaultMaxDepth is how deep spawn chains go: the orchestrator's tasks
	// are depth 1, their sub-tasks depth 2, and so on.
	DefaultMaxDepth = 3
	// DefaultMaxChildren caps a task's working children. The orchestrator's
	// are capped by concurrency instead.
	DefaultMaxChildren = 8
)

// spawnCaps returns the max spawn depth and the max working children per
// task, from [spawn]; 0 means no cap.
func (a *App) spawnCaps() (maxDepth, maxChildren int) {
	return a.Cfg.Spawn.MaxDepth, a.Cfg.Spawn.MaxChildren
}

// depth is how far below the orchestrator a task sits; the orchestrator and
// tasks with no parent are depth 0.
func (a *App) depth(id string) int {
	d := 0
	for seen := map[string]bool{}; id != "" && id != OrchestratorID && !seen[id]; d++ {
		seen[id] = true
		t, err := a.Store.Task(id)
		if err != nil {
			return d + 1
		}
		id = t.Parent
	}
	return d
}

// checkSpawnCaps refuses a spawn that would nest deeper than the max depth or
// give parent more working children than allowed.
func (a *App) checkSpawnCaps(parent string) error {
	if parent == "" || parent == OrchestratorID {
		return nil
	}
	maxDepth, maxChildren := a.spawnCaps()
	if d := a.depth(parent) + 1; maxDepth > 0 && d > maxDepth {
		return fmt.Errorf("spawn depth cap: a sub-task of %s would be at depth %d, over the max of %d; do the work yourself or ask the orchestrator to spawn it", parent, d, maxDepth)
	}
	ts, err := a.Store.Tasks()
	if err != nil {
		return err
	}
	n := 0
	for _, t := range ts {
		if t.Parent == parent && (t.Status == store.Running || t.Status == store.Idle || t.Status == store.NeedsYou) {
			n++
		}
	}
	if maxChildren > 0 && n >= maxChildren {
		return fmt.Errorf("spawn fan-out cap: %s already has %d working children (max %d); wait for one to finish", parent, n, maxChildren)
	}
	return nil
}
