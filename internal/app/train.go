package app

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/brandonapol/saddle/internal/claims"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

type LandResult struct {
	Task  string `json:"task"`
	State string `json:"state"`
	Note  string `json:"note,omitempty"`
}

// Land runs the merge train: each queued branch, in order, is rebased onto the
// integration branch, tested, and fast-forwarded in. Failures go back to the
// producing agent and the train moves on.
func (a *App) Land() ([]LandResult, error) {
	if err := a.Init(); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(a.stateDir("train.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	// Closing the file also drops the lock, so a failed unlock is harmless.
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()

	if br, _ := gitx.CurrentBranch(a.Root); br == a.Cfg.Integration {
		return nil, fmt.Errorf("%s is checked out in %s; switch it to another branch so the train can move it", a.Cfg.Integration, a.Root)
	}
	entries, err := a.Store.Train()
	if err != nil {
		return nil, err
	}
	var out []LandResult
	for _, e := range entries {
		if e.State != store.Queued {
			continue
		}
		r := a.landOne(e.Task)
		out = append(out, r)
	}
	return out, nil
}

func (a *App) landOne(id string) LandResult {
	res := LandResult{Task: id}
	fail := func(state, note, msg string) LandResult {
		res.State, res.Note = state, note
		a.Store.Event(id, "train_"+state, note)
		if err := errors.Join(
			a.Store.SetTrain(id, state, note, true),
			a.Store.SetStatus(id, store.Conflict),
			a.Notify(id, store.NoticeAction, msg),
		); err != nil {
			res.Note += " (could not record or notify: " + err.Error() + ")"
		}
		return res
	}
	t, err := a.Store.Task(id)
	if err != nil {
		res.State, res.Note = store.TrainError, err.Error()
		return res
	}
	if dirty, _ := gitx.Dirty(t.Worktree); len(dirty) > 0 {
		return fail(store.TrainError, "uncommitted changes",
			"Your branch could not land because the worktree has uncommitted changes. Commit everything, then call the saddle done tool again.")
	}
	old, err := gitx.RevParse(a.Root, a.Cfg.Integration)
	if err != nil {
		res.State, res.Note = store.TrainError, err.Error()
		return res
	}
	rr, err := gitx.Rebase(t.Worktree, a.Cfg.Integration, true)
	if err != nil {
		return fail(store.TrainError, "rebase failed", "Rebasing your branch onto "+a.Cfg.Integration+" failed:\n"+rr.Output+"\nFix it and call done again.")
	}
	if !rr.OK {
		files := strings.Join(rr.Conflicts, ", ")
		return fail(store.TrainError, files, fmt.Sprintf(
			"Your branch could not land: rebasing onto %s conflicts in %s. Other work already landed there, so this conflict is yours to fix.\n"+
				"1. Run `saddle sync`. It starts the rebase and stops at the conflicts.\n"+
				"2. Resolve them, keeping both sides' intent, then `git add` and `git rebase --continue`.\n"+
				"3. Run the tests, then call the saddle done tool again.", a.Cfg.Integration, files))
	}
	if cmd := a.Cfg.Test.Cmd; cmd != "" {
		if out, err := runShell(t.Worktree, cmd); err != nil {
			return fail(store.TestFailed, "tests failed", fmt.Sprintf(
				"Your branch rebased cleanly onto %s, but `%s` failed on the result:\n%s\nFix it, commit, and call the saddle done tool again.", a.Cfg.Integration, cmd, tail(out, 40)))
		}
	}
	head, err := gitx.RevParse(t.Worktree, "HEAD")
	if err != nil {
		res.State, res.Note = store.TrainError, err.Error()
		return res
	}
	if err := gitx.UpdateRef(a.Root, a.Cfg.Integration, head, old); err != nil {
		// The entry is still queued, so the next land retries it.
		res.State, res.Note = store.TrainError, "integration moved during land; will retry: "+err.Error()
		return res
	}
	res.State, res.Note = store.TrainOK, head[:12]
	a.Store.Event(id, "landed", head)
	if err := errors.Join(
		a.Store.SetTrain(id, store.TrainOK, head[:12], false),
		a.Store.SetStatus(id, store.Landed),
	); err != nil {
		res.Note += " (landed, but state not saved: " + err.Error() + ")"
	}

	if err := a.broadcastLanding(t, old, head); err != nil {
		res.Note += " (broadcast incomplete: " + err.Error() + ")"
	}

	if a.Cfg.CloseOnLand {
		// Best-effort cleanup: the landed branch is what matters.
		if t.Window != "" && a.Tmux.Alive(t.Window) {
			_ = a.Tmux.KillWindow(t.Window)
		}
		_ = gitx.WorktreeRemove(a.Root, t.Worktree)
	}
	if err := a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf("%s %q landed on %s at %s.", id, t.Title, a.Cfg.Integration, head[:12])); err != nil {
		res.Note += " (orchestrator not notified: " + err.Error() + ")"
	}
	return res
}

// broadcastLanding records renames, remaps the other tasks' claims through
// them, and tells every live agent what moved under it.
func (a *App) broadcastLanding(landed store.Task, old, head string) error {
	grs, _ := gitx.Renames(a.Root, old, head)
	changed, _ := gitx.ChangedFiles(a.Root, old, head)
	var srs []store.Rename
	var crs []claims.Rename
	for _, r := range grs {
		srs = append(srs, store.Rename{Old: r.Old, New: r.New})
		crs = append(crs, claims.Rename{Old: r.Old, New: r.New})
	}
	var errs []error
	errs = append(errs, a.Store.AddRenames(landed.ID, srs))
	all, err := a.Store.Claims()
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	ts, err := a.Store.Tasks()
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, t := range ts {
		if t.ID == landed.ID || t.Role != store.RoleWorker || !t.Active() {
			continue
		}
		var msg strings.Builder
		fmt.Fprintf(&msg, "%s %q landed on %s (%d files changed).", landed.ID, landed.Title, a.Cfg.Integration, len(changed))
		if len(grs) > 0 {
			msg.WriteString(" It moved files:")
			for i, r := range grs {
				if i == 15 {
					fmt.Fprintf(&msg, "\n  … %d more", len(grs)-15)
					break
				}
				fmt.Fprintf(&msg, "\n  %s → %s", r.Old, r.New)
			}
		}
		var remapped []string
		for _, c := range all[t.ID] {
			if nc := claims.Remap(c, crs); nc != c {
				if err := a.Store.ReplaceClaim(t.ID, c, nc); err != nil {
					errs = append(errs, err)
					continue
				}
				remapped = append(remapped, c+" → "+nc)
			}
		}
		if len(remapped) > 0 {
			msg.WriteString("\nYour claims were remapped: " + strings.Join(remapped, ", ") + ".")
		}
		kind := store.NoticeInfo
		if overlaps(t.Worktree, old, changed) {
			kind = store.NoticeAction
			msg.WriteString("\nIt touched files you changed too, so sync now to avoid a conflict later.")
		}
		msg.WriteString("\nAt your next clean point (commit first), run `saddle sync` to rebase onto it. Directory moves are followed automatically.")
		errs = append(errs, a.Notify(t.ID, kind, msg.String()))
	}
	return errors.Join(errs...)
}

// overlaps reports whether the worktree's branch changed any of files.
func overlaps(wt, base string, files []string) bool {
	out, err := gitx.Run(wt, "diff", "--name-only", base+"...HEAD")
	if err != nil || out == "" {
		return false
	}
	mine := map[string]bool{}
	for _, f := range strings.Split(out, "\n") {
		mine[f] = true
	}
	for _, f := range files {
		if mine[f] {
			return true
		}
	}
	return false
}

// Sync rebases a task's branch onto the integration branch. Conflicts are left
// in place for the agent to resolve.
func (a *App) Sync(task string) (gitx.RebaseResult, error) {
	t, err := a.Store.Task(task)
	if err != nil {
		return gitx.RebaseResult{}, err
	}
	if dirty, _ := gitx.Dirty(t.Worktree); len(dirty) > 0 {
		return gitx.RebaseResult{}, fmt.Errorf("commit your changes before syncing:\n%s", strings.Join(dirty, "\n"))
	}
	rr, err := gitx.Rebase(t.Worktree, a.Cfg.Integration, false)
	a.Store.Event(task, "sync", fmt.Sprintf("ok=%v conflicts=%s", rr.OK, strings.Join(rr.Conflicts, ",")))
	return rr, err
}

// PRs pushes every landed branch and opens or updates a stack of PRs: the
// first targets base, each later one targets the branch landed before it.
func (a *App) PRs() ([]string, error) {
	entries, err := a.Store.Train()
	if err != nil {
		return nil, err
	}
	var stack []store.Task
	for _, e := range entries {
		if e.State == store.TrainOK {
			t, err := a.Store.Task(e.Task)
			if err != nil {
				return nil, err
			}
			stack = append(stack, t)
		}
	}
	if len(stack) == 0 {
		return nil, fmt.Errorf("nothing has landed yet")
	}
	base := a.Cfg.Base
	for i := range stack {
		t := &stack[i]
		if _, err := gitx.Run(a.Root, "push", "--force-with-lease", "-u", "origin", t.Branch); err != nil {
			return nil, err
		}
		if t.PR == "" {
			url, err := gh(a.Root, "pr", "create", "--base", base, "--head", t.Branch, "--title", t.Title, "--body", t.Summary)
			if err != nil {
				return nil, err
			}
			t.PR = lastLine(url)
			if err := a.Store.SetField(t.ID, "pr", t.PR); err != nil {
				return nil, err
			}
		} else if _, err := gh(a.Root, "pr", "edit", t.PR, "--base", base); err != nil {
			return nil, err
		}
		base = t.Branch
	}
	var urls []string
	for i, t := range stack {
		var b strings.Builder
		b.WriteString(t.Summary)
		if t.Issue > 0 {
			fmt.Fprintf(&b, "\n\nCloses #%d", t.Issue)
		}
		b.WriteString("\n\n---\n**Stack** (opened by saddle; merge bottom-up)\n\n")
		for j := len(stack) - 1; j >= 0; j-- {
			mark := ""
			if j == i {
				mark = " 👈"
			}
			fmt.Fprintf(&b, "%d. %s %s%s\n", j+1, stack[j].PR, stack[j].Title, mark)
		}
		b.WriteString("\nBase: `" + a.Cfg.Base + "`\n")
		if _, err := gh(a.Root, "pr", "edit", t.PR, "--body", b.String()); err != nil {
			return urls, err
		}
		urls = append(urls, t.PR)
	}
	return urls, nil
}

func gh(dir string, args ...string) (string, error) {
	cmd := exec.Command("gh", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gh %s: %w: %s", strings.Join(args[:2], " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func runShell(dir, cmd string) (string, error) {
	c := exec.Command("sh", "-c", cmd)
	c.Dir = dir
	out, err := c.CombinedOutput()
	return string(out), err
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
