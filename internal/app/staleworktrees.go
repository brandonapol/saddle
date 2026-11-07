package app

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// StaleWorktrees counts the stale worktrees: directories under the worktree
// directory, named after a task, whose task has ended (landed, killed, no
// change or failed) or is gone from the store. saddle gc removes them (#803).
// It reads the directory and the store only, so status can call it often.
func (a *App) StaleWorktrees() (int, error) {
	es, err := os.ReadDir(a.stateDir("worktrees"))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	ts, err := a.Store.Tasks()
	if err != nil {
		return 0, err
	}
	live := map[string]bool{}
	for _, t := range ts {
		if t.Active() {
			live[t.ID] = true
		}
	}
	n := 0
	for _, e := range es {
		id := taskIDRe.FindString(e.Name())
		if !e.IsDir() || id == "" || live[id] {
			continue
		}
		if _, err := os.Lstat(filepath.Join(a.stateDir("worktrees"), e.Name(), ".git")); err == nil {
			n++
		}
	}
	return n, nil
}
