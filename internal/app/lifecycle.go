package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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

// Warnings lists problems the human and the orchestrator should see in status,
// including landed branches that drifted off their landed commit.
// It uses the last fetched remote refs and does not fetch.
func (a *App) Warnings() []string {
	var ws []string
	if r := a.remote(); r != "" && gitx.BranchExists(a.Root, a.Cfg.Integration) {
		if w := a.integrationBehind(r + "/" + a.Cfg.Base); w != "" {
			ws = append(ws, w)
		}
	}
	// A landed branch that moved off its landed commit would make its PR lie.
	drift, err := a.Drift()
	if err != nil {
		drift = []string{"can't check landed branches for drift: " + err.Error()}
	}
	return append(ws, drift...)
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
	Kind string `json:"kind"` // worktree, branch, remote (a <remote>/saddle/* branch) or ref
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

// PRHead is what GitHub says about where a PR went and the head it merged.
type PRHead struct {
	State       string `json:"state"`       // OPEN, CLOSED or MERGED
	BaseRefName string `json:"baseRefName"` // the branch it targets, or was merged into
	HeadRefOid  string `json:"headRefOid"`  // the commit it merged
}

// prHead asks GitHub about the PR at url through gh, run in dir. Tests replace it.
var prHead = func(dir, url string) (PRHead, error) {
	var pr PRHead
	out, err := gh(dir, "pr", "view", url, "--json", "state,baseRefName,headRefOid")
	if err != nil {
		return pr, err
	}
	if err := json.Unmarshal([]byte(out), &pr); err != nil {
		return pr, fmt.Errorf("gh pr view %s: %w", url, err)
	}
	return pr, nil
}

// unmerged says why tip holds work gc must keep, or "" when all of it has
// landed: tip is on integration or base, its own commits are on base under
// other SHAs (a squash or rebase merge, found by tree or patch-id), every file
// it changed already matches base, or its task's PR merged into base at tip.
// SHA reachability alone misses squash merges (#158).
func (a *App) unmerged(tip string, t store.Task, base string) string {
	if ancestor(a.Root, tip, a.Cfg.Integration) || ancestor(a.Root, tip, base) {
		return ""
	}
	mb, err := gitx.Run(a.Root, "merge-base", tip, base)
	if err != nil {
		return "can't compare with " + a.Cfg.Base + ": " + err.Error()
	}
	if files, err := gitx.Run(a.Root, "diff", "--name-only", "--no-renames", mb, tip); err == nil && files != "" {
		args := append([]string{"diff", "--quiet", tip, base, "--"}, strings.Split(files, "\n")...)
		if _, err := gitx.Run(a.Root, args...); err == nil {
			return ""
		}
	}
	c, err := gitx.Run(a.Root, "rev-list", "--count", "--first-parent", mb+".."+tip)
	if err != nil {
		return err.Error()
	}
	own, err := strconv.Atoi(c)
	if err != nil {
		return err.Error()
	}
	_, held, err := gitx.LandedPrefixOf(a.Root, tip, base)
	if err != nil {
		return "can't compare with " + a.Cfg.Base + ": " + err.Error()
	}
	if held == own || a.prMerged(t, tip) {
		return ""
	}
	return fmt.Sprintf("%d commit(s) not on %s or %s", own-held, a.Cfg.Integration, a.Cfg.Base)
}

// prMerged reports whether t's PR merged into base with tip's work in it.
func (a *App) prMerged(t store.Task, tip string) bool {
	if t.PR == "" {
		return false
	}
	pr, err := prHead(a.Root, t.PR)
	if err != nil || pr.State != "MERGED" || (pr.BaseRefName != "" && pr.BaseRefName != a.Cfg.Base) {
		return false
	}
	return pr.HeadRefOid == tip || ancestor(a.Root, tip, pr.HeadRefOid)
}

func ancestor(dir, a, b string) bool {
	_, err := gitx.Run(dir, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

// Leftovers lists worktrees, saddle/* branches (local and on the remote) and
// refs/saddle/* refs that belong to no live task: killed, failed or landed
// tasks, or none at all. Branches whose work hasn't landed, the remote copy of
// a branch whose PR is still in the stack, and worktrees with uncommitted
// changes are listed with why gc keeps them. It reads the last fetched refs.
func (a *App) Leftovers() ([]Leftover, error) {
	ts, err := a.Store.Tasks()
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	byID := map[string]store.Task{}
	for _, t := range ts {
		byID[t.ID] = t
		if t.Active() && t.Status != StatusFailed {
			live[t.ID] = true
		}
	}
	stacked := map[string]bool{}
	es, err := a.Store.Train()
	if err != nil {
		return nil, err
	}
	for _, e := range es {
		if e.State == store.TrainOK {
			stacked[e.Task] = true
		}
	}
	base := a.baseRef()
	task := func(name string) store.Task { return byID[taskIDRe.FindString(name)] }
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
		if why := a.unmerged("refs/heads/"+b, task(strings.TrimPrefix(b, "saddle/")), base); why != "" {
			l.Keep = why + "; delete with git branch -D"
		}
		out = append(out, l)
	}
	if r := a.remote(); r != "" {
		rbs, err := gitx.Run(a.Root, "for-each-ref", "--format=%(refname:short)", "refs/remotes/"+r+"/saddle/")
		if err != nil {
			return nil, err
		}
		for _, rb := range strings.Fields(rbs) {
			name := strings.TrimPrefix(rb, r+"/saddle/")
			if owned(name) {
				continue
			}
			l := Leftover{Kind: "remote", Name: rb}
			t := task(name)
			if stacked[t.ID] {
				l.Keep = "its PR is still in the stack"
			} else if why := a.unmerged("refs/remotes/"+rb, t, base); why != "" {
				l.Keep = why
			}
			out = append(out, l)
		}
	}
	refs, err := gitx.Run(a.Root, "for-each-ref", "--format=%(refname)", "refs/saddle/")
	if err != nil {
		return nil, err
	}
	for _, r := range strings.Fields(refs) {
		if !owned(filepath.Base(r)) {
			l := Leftover{Kind: "ref", Name: r}
			// A snapshot of uncommitted work (#254) stays until that work lands.
			if strings.HasPrefix(r, wipRef("")) {
				if why := a.unmerged(r, task(filepath.Base(r)), base); why != "" {
					l.Keep = "snapshot of uncommitted work; delete with git update-ref -d"
				}
			}
			out = append(out, l)
		}
	}
	return out, nil
}

// GCCounts counts what gc would remove and what it would keep. Doctor uses
// it, so its warning matches gc and clears once gc has run.
func (a *App) GCCounts() (remove, kept int, err error) {
	ls, err := a.Leftovers()
	for _, l := range ls {
		if l.Keep != "" {
			kept++
		} else {
			remove++
		}
	}
	return remove, kept, err
}

// gcFetch refreshes base and the remote's saddle/* branches, dropping those
// the remote deleted, so gc judges branches against what has really merged.
func (a *App) gcFetch() {
	r := a.remote()
	if r == "" {
		return
	}
	if _, err := gitx.Run(a.Root, "fetch", "--quiet", "--prune", r,
		"+refs/heads/"+a.Cfg.Base+":refs/remotes/"+r+"/"+a.Cfg.Base,
		"+refs/heads/saddle/*:refs/remotes/"+r+"/saddle/*"); err != nil {
		a.Store.Event("", "fetch_failed", err.Error())
	}
}

// deleteRemote deletes a remote saddle/* branch, given as <remote>/<branch>.
// A branch the remote already deleted only loses its tracking ref.
func (a *App) deleteRemote(name string) error {
	r := a.remote()
	branch, ok := strings.CutPrefix(name, r+"/")
	if r == "" || !ok {
		return fmt.Errorf("%s is not on remote %q", name, r)
	}
	_, err := trainGit(a.Root, "push", "--quiet", r, "--delete", "refs/heads/"+branch)
	if err == nil {
		return nil
	}
	if out, lerr := gitx.Run(a.Root, "ls-remote", r, "refs/heads/"+branch); lerr != nil || out != "" {
		return err
	}
	_, err = gitx.Run(a.Root, "update-ref", "-d", "refs/remotes/"+name)
	return err
}

// GC fetches, then removes every leftover it safely can. It returns what it
// removed and what it kept, stopping at the first error.
func (a *App) GC() ([]Leftover, error) {
	a.gcFetch()
	ls, err := a.Leftovers()
	if err != nil {
		return nil, err
	}
	var out []Leftover
	for _, l := range ls {
		if l.Keep == "" {
			switch l.Kind {
			case "worktree":
				if id := taskIDRe.FindString(filepath.Base(l.Name)); id != "" {
					a.snapshotWIP(id, l.Name)
				}
				err = gitx.WorktreeRemove(a.Root, l.Name)
			case "branch":
				_, err = gitx.Run(a.Root, "branch", "-D", l.Name)
			case "remote":
				err = a.deleteRemote(l.Name)
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
