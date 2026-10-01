package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/tmux"
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
		// The task is still live, so only the train may delete its branch.
		_, err := trainGit(a.Root, "branch", "-D", t.Branch)
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

// Leftover is a worktree, branch or ref that no live task uses.
type Leftover struct {
	Kind string `json:"kind"` // worktree, branch or ref
	Name string `json:"name"`
	Keep string `json:"keep,omitempty"` // why gc won't remove it
}

// cleanup removes a dead task's worktree and, when it holds no commits
// beyond integration, its branch. It says what it kept and why.
func (a *App) cleanup(t store.Task) (string, error) {
	if _, err := os.Stat(t.Worktree); err == nil {
		if dirty, _ := gitx.Dirty(t.Worktree); len(dirty) > 0 {
			return "kept worktree: uncommitted changes", nil
		}
		if err := gitx.WorktreeRemove(a.Root, t.Worktree); err != nil {
			return "", err
		}
	}
	if t.Branch == "" || !gitx.BranchExists(a.Root, t.Branch) {
		return "", nil
	}
	if n := a.unique(t.Branch); n > 0 {
		return fmt.Sprintf("kept branch %s: %d commit(s) not on integration", t.Branch, n), nil
	}
	_, err := gitx.Run(a.Root, "branch", "-D", t.Branch)
	return "", err
}

// unique counts commits on branch that integration lacks (-1 if unknown).
func (a *App) unique(branch string) int {
	n, err := gitx.CommitsBetween(a.Root, a.Cfg.Integration, branch)
	if err != nil {
		return -1
	}
	return n
}

var taskIDRe = regexp.MustCompile(`^t[0-9]+\b`)

// Leftovers lists worktrees, saddle/* branches and refs/saddle/* refs that
// belong to no live task: killed, failed or landed tasks, or none at all.
func (a *App) Leftovers() ([]Leftover, error) {
	ts, err := a.Store.Tasks()
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	for _, t := range ts {
		if t.Active() && t.Status != StatusFailed {
			live[t.ID] = true
		}
	}
	// Names are <id>-<slug> (worktree dirs, branches) or end in <id> (refs).
	// Anything not named after a task, like saddle/integration, isn't ours to remove.
	owned := func(name string) bool {
		id := taskIDRe.FindString(name)
		return id == "" || live[id]
	}
	if _, err := gitx.Run(a.Root, "worktree", "prune"); err != nil {
		return nil, err
	}
	var out []Leftover
	inUse := map[string]bool{} // branches checked out in worktrees gc keeps
	wts, err := gitx.Run(a.Root, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	dir := a.stateDir("worktrees") + string(filepath.Separator)
	for _, block := range strings.Split(wts, "\n\n") {
		var path, branch string
		for _, line := range strings.Split(block, "\n") {
			if p, ok := strings.CutPrefix(line, "worktree "); ok {
				path = p
			} else if b, ok := strings.CutPrefix(line, "branch refs/heads/"); ok {
				branch = b
			}
		}
		if !strings.HasPrefix(path, dir) || owned(filepath.Base(path)) {
			inUse[branch] = true
			continue
		}
		l := Leftover{Kind: "worktree", Name: path}
		if dirty, _ := gitx.Dirty(path); len(dirty) > 0 {
			l.Keep = "uncommitted changes"
			inUse[branch] = true
		}
		out = append(out, l)
	}
	brs, err := gitx.Run(a.Root, "for-each-ref", "--format=%(refname:short)", "refs/heads/saddle/")
	if err != nil {
		return nil, err
	}
	for _, b := range strings.Fields(brs) {
		if inUse[b] || owned(strings.TrimPrefix(b, "saddle/")) {
			continue
		}
		l := Leftover{Kind: "branch", Name: b}
		if n := a.unique(b); n != 0 {
			l.Keep = fmt.Sprintf("%d commit(s) not on integration; delete with git branch -D", n)
		}
		out = append(out, l)
	}
	refs, err := gitx.Run(a.Root, "for-each-ref", "--format=%(refname)", "refs/saddle/")
	if err != nil {
		return nil, err
	}
	for _, r := range strings.Fields(refs) {
		if !owned(filepath.Base(r)) {
			out = append(out, Leftover{Kind: "ref", Name: r})
		}
	}
	return out, nil
}

// GC removes every leftover it safely can. It returns what it removed and
// what it kept, stopping at the first error.
func (a *App) GC() ([]Leftover, error) {
	ls, err := a.Leftovers()
	if err != nil {
		return nil, err
	}
	var out []Leftover
	for _, l := range ls {
		if l.Keep == "" {
			switch l.Kind {
			case "worktree":
				err = gitx.WorktreeRemove(a.Root, l.Name)
			case "branch":
				_, err = gitx.Run(a.Root, "branch", "-D", l.Name)
			case "ref":
				_, err = gitx.Run(a.Root, "update-ref", "-d", l.Name)
			}
			if err != nil {
				return out, err
			}
			a.Store.Event("", "gc", l.Kind+" "+l.Name)
		}
		out = append(out, l)
	}
	return out, nil
}

// windowNamer is implemented by tmux drivers that can report a window's name.
type windowNamer interface {
	WindowName(id string) (string, error)
}

var _ windowNamer = tmux.Tmux{}

// ownWindow reports whether t's window is one saddle opened for it: a live
// worker window still named after the task. Keys are never typed anywhere
// else; window ids outlive the windows saddle made and get reused.
func (a *App) ownWindow(t store.Task) bool {
	if t.Role != store.RoleWorker || t.Window == "" || !a.Tmux.Alive(t.Window) {
		return false
	}
	if n, ok := a.Tmux.(windowNamer); ok {
		name, err := n.WindowName(t.Window)
		return err == nil && strings.HasPrefix(name, t.ID+"-")
	}
	return true
}
