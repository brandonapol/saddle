package app

import (
	"github.com/brandonapol/saddle/internal/checkpoint"
	"github.com/brandonapol/saddle/internal/store"
)

// NewCheckpointWatcher checkpoints live workers' uncommitted work to
// refs/saddle/checkpoints/<task> and nudges them to commit (#50). saddle up
// and the plugin engine run it.
func (a *App) NewCheckpointWatcher() *checkpoint.Watcher {
	return checkpoint.NewWatcher(a.Root, checkpointHost{a}, checkpoint.DefaultPolicy)
}

type checkpointHost struct{ a *App }

func (h checkpointHost) Targets() ([]checkpoint.Target, error) {
	ts, err := h.a.Store.Tasks()
	if err != nil {
		return nil, err
	}
	var out []checkpoint.Target
	for _, t := range ts {
		if t.Role == store.RoleWorker && t.Worktree != "" && (t.Status == store.Running || t.Status == store.Idle || t.Status == store.NeedsYou) {
			out = append(out, checkpoint.Target{Task: t.ID, Worktree: t.Worktree})
		}
	}
	return out, nil
}

func (h checkpointHost) Nudge(task, text string) error {
	return h.a.Notify(task, store.NoticeInfo, text)
}

func (h checkpointHost) Event(task, kind, detail string) { h.a.Store.Event(task, kind, detail) }

// pruneCheckpoint drops task's checkpoint once its work has landed or been
// killed (kill snapshots uncommitted work to refs/saddle/wip/ first).
func (a *App) pruneCheckpoint(task string) {
	if err := checkpoint.Prune(a.Root, task); err != nil {
		a.Store.Event(task, "checkpoint_prune_failed", err.Error())
	}
}
