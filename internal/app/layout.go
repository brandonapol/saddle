package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/brandonapol/saddle/internal/gitx"
)

// openLayout makes a fresh detached worktree at tip under .saddle, named for
// this run alone, the scratch checkout the PR layout replays commits in. Runs
// that overlap (the automerge loop and a prs run) each get their own, so one
// can't delete the other's (#680). The old shared .saddle/layout is cleared
// of any stale registration (its directory gone, maybe left locked by a run
// that died mid cherry-pick); no other worktree is pruned (#669).
func (a *App) openLayout(tip string) (string, error) {
	legacy := a.stateDir("layout")
	_ = gitx.WorktreeRemove(a.Root, legacy)
	if err := os.RemoveAll(legacy); err != nil {
		return "", err
	}
	if err := a.forgetLayoutWorktree(legacy); err != nil {
		return "", err
	}
	if err := os.MkdirAll(a.stateDir(), 0o755); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(a.stateDir(), "layout-")
	if err != nil {
		return "", err
	}
	if _, err := gitx.Run(a.Root, "worktree", "add", "--detach", dir, tip); err != nil {
		return "", err
	}
	return dir, nil
}

// forgetLayoutWorktree deletes the .git/worktrees entries registered for
// dir, and only those.
func (a *App) forgetLayoutWorktree(dir string) error {
	common, err := gitx.Run(a.Root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(common, "worktrees"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	want := canonPath(filepath.Join(dir, ".git"))
	for _, e := range entries {
		entry := filepath.Join(common, "worktrees", e.Name())
		raw, err := os.ReadFile(filepath.Join(entry, "gitdir"))
		if err != nil || canonPath(strings.TrimSpace(string(raw))) != want {
			continue
		}
		if err := os.RemoveAll(entry); err != nil {
			return err
		}
	}
	return nil
}

// canonPath resolves symlinks in p's deepest existing ancestor, so a path
// whose directory is gone still compares equal to its registered spelling.
func canonPath(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for d := p; ; d = filepath.Dir(d) {
		if r, err := filepath.EvalSymlinks(d); err == nil {
			return filepath.Join(r, rest)
		}
		if filepath.Dir(d) == d {
			return p
		}
		rest = filepath.Join(filepath.Base(d), rest)
	}
}

// layoutReplay replays from..to onto tip in the layout worktree *dir,
// opening it on first use. When the replay fails, say because the directory
// vanished after a merge or its worktree entry went stale, the worktree is
// rebuilt and the replay retried once before the error stops the caller
// (#669).
func (a *App) layoutReplay(dir *string, tip, from, to string) (string, bool, error) {
	if a.previewLayout {
		return a.replayLayout("", tip, from, to)
	}
	if *dir == "" {
		d, err := a.openLayout(tip)
		if err != nil {
			return "", false, err
		}
		*dir = d
	}
	head, ok, err := replayOnto(*dir, tip, from, to)
	if err == nil {
		return head, ok, nil
	}
	_ = gitx.WorktreeRemove(a.Root, *dir)
	_ = os.RemoveAll(*dir)
	d, oerr := a.openLayout(tip)
	if oerr != nil {
		*dir = ""
		return "", false, fmt.Errorf("%w (rebuilding the layout worktree: %v)", err, oerr)
	}
	*dir = d
	return replayOnto(d, tip, from, to)
}

// settleLayout aborts a cherry-pick left in progress in dir, so the next
// checkout starts from a clean index.
func settleLayout(dir string) {
	if _, err := gitx.RevParse(dir, "CHERRY_PICK_HEAD"); err != nil {
		return
	}
	if _, err := trainGit(dir, "cherry-pick", "--abort"); err == nil {
		return
	}
	_, _ = trainGit(dir, "cherry-pick", "--quit")
	_, _ = trainGit(dir, "reset", "-q", "--hard")
}
