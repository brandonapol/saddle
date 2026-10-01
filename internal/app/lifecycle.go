package app

import (
	"errors"
	"fmt"
	"strings"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// remote is the remote base tracks: its configured upstream, else origin.
// Empty means the repo has no remote to fetch from.
func (a *App) remote() string {
	if r, err := gitx.Run(a.Root, "config", "branch."+a.Cfg.Base+".remote"); err == nil && r != "" && r != "." {
		return r
	}
	rs, _ := gitx.Run(a.Root, "remote")
	for _, r := range strings.Fields(rs) {
		if r == "origin" {
			return r
		}
	}
	return ""
}

// freshBase fetches base from its remote and returns the ref to cut from:
// <remote>/<base>, or the local base when there is no remote copy.
func (a *App) freshBase() string {
	r := a.remote()
	if r == "" {
		return a.Cfg.Base
	}
	if _, err := gitx.Run(a.Root, "fetch", "--quiet", r, a.Cfg.Base); err != nil {
		a.Store.Event("", "fetch_failed", err.Error())
	}
	ref := r + "/" + a.Cfg.Base
	if _, err := gitx.RevParse(a.Root, ref); err != nil {
		return a.Cfg.Base
	}
	return ref
}

// integrationBehind reports how many commits base has that integration lacks,
// as a warning, or "" when integration is up to date.
func (a *App) integrationBehind(base string) string {
	n, err := gitx.CommitsBetween(a.Root, a.Cfg.Integration, base)
	if err != nil || n == 0 {
		return ""
	}
	return fmt.Sprintf("integration behind %s by %d", base, n)
}

// checkBehind records one integration_behind event, and tells the
// orchestrator, each time the gap changes. Agents must not fix it themselves.
func (a *App) checkBehind(base string) {
	w := a.integrationBehind(base)
	if w == "" {
		return
	}
	es, _ := a.Store.Events(500)
	for i := len(es) - 1; i >= 0; i-- {
		if es[i].Kind == "integration_behind" {
			if es[i].Data == w {
				return
			}
			break
		}
	}
	a.Store.Event("", "integration_behind", w)
	_ = a.Store.Notify(OrchestratorID, store.NoticeInfo,
		w+". New tasks still start from integration. Restack it onto "+base+"; don't merge base into task branches.")
}

// Warnings lists problems the human and the orchestrator should see in status.
// It uses the last fetched remote refs and does not fetch.
func (a *App) Warnings() []string {
	var ws []string
	if r := a.remote(); r != "" && gitx.BranchExists(a.Root, a.Cfg.Integration) {
		if w := a.integrationBehind(r + "/" + a.Cfg.Base); w != "" {
			ws = append(ws, w)
		}
	}
	return ws
}

// LocalBaseBehind fetches and warns when the local base branch is behind its
// upstream, so the human knows their checkout is stale.
func (a *App) LocalBaseBehind() string {
	up := a.freshBase()
	if up == a.Cfg.Base {
		return ""
	}
	n, err := gitx.CommitsBetween(a.Root, a.Cfg.Base, up)
	if err != nil || n == 0 {
		return ""
	}
	return fmt.Sprintf("local %s is %d commit(s) behind %s; saddle cuts work from %s", a.Cfg.Base, n, up, up)
}

// StatusFailed marks a task whose spawn failed. Its id stays burned.
const StatusFailed = "failed"

// spawnFailed undoes a half-made spawn and records why it failed. The task
// row stays (status failed, reason in its summary) so the id is not reused
// and the failure doesn't look like a kill.
func (a *App) spawnFailed(t store.Task, worktree, hadBranch bool, cause error) error {
	var errs []error
	if worktree {
		errs = append(errs, gitx.WorktreeRemove(a.Root, t.Worktree))
	}
	if !hadBranch && gitx.BranchExists(a.Root, t.Branch) {
		_, err := gitx.Run(a.Root, "branch", "-D", t.Branch)
		errs = append(errs, err)
	}
	reason := cause.Error()
	a.Store.Event(t.ID, "spawn_failed", reason)
	errs = append(errs, a.Store.Release(t.ID), a.Store.SetField(t.ID, "summary", reason), a.Store.SetStatus(t.ID, StatusFailed))
	return errors.Join(append([]error{fmt.Errorf("%s failed: %w", t.ID, cause)}, errs...)...)
}

// retryHint names the latest failed spawn with the same title, so a retry
// says what went wrong last time.
func (a *App) retryHint(title string) string {
	ts, err := a.Store.Tasks()
	if err != nil {
		return ""
	}
	hint := ""
	for _, t := range ts {
		if t.Status == StatusFailed && t.Title == title {
			hint = fmt.Sprintf(" retry: %s failed: %s", t.ID, t.Summary)
		}
	}
	return hint
}
