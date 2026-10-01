package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	if strings.TrimSpace(a.Cfg.Test.Cmd) == "" {
		return nil, errNoTestCmd
	}
	if err := a.checkFlag(); err != nil {
		return nil, err
	}
	unlock, err := a.lockTrain()
	if err != nil {
		return nil, err
	}
	defer unlock()

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

// lockTrain takes the train lock, which serialises everything that moves the
// integration branch or landed task branches.
func (a *App) lockTrain() (unlock func(), err error) {
	lock, err := os.OpenFile(a.stateDir("train.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		lock.Close()
		return nil, err
	}
	// Closing the file also drops the lock, so a failed unlock is harmless.
	return func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		lock.Close()
	}, nil
}

// TryLockTrain takes the train lock if nobody holds it. ok is false when the
// train is busy, so a watcher can skip a cycle instead of waiting out a land.
func (a *App) TryLockTrain() (unlock func(), ok bool, err error) {
	lock, err := os.OpenFile(a.stateDir("train.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		lock.Close()
	}, true, nil
}

// StackFlag marks the PR stack as at risk. While it is set, prs and land
// refuse to push or open anything on top of the stack. The stack sentinel
// sets it and clears it once the stack checks clean; restack is the fix.
type StackFlag struct {
	Task  string   `json:"task"`  // the first broken task
	Cause string   `json:"cause"` // why the stack is at risk
	PRs   []string `json:"prs"`   // PRs labeled for it
}

func (a *App) flagPath() string { return a.stateDir("stack-at-risk.json") }

// Flag returns the stack's at-risk flag, if one is set.
func (a *App) Flag() (StackFlag, bool, error) {
	var f StackFlag
	b, err := os.ReadFile(a.flagPath())
	if errors.Is(err, os.ErrNotExist) {
		return f, false, nil
	}
	if err != nil {
		return f, false, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, false, fmt.Errorf("%s: %w", a.flagPath(), err)
	}
	return f, true, nil
}

// SetFlag flags the stack as at risk.
func (a *App) SetFlag(f StackFlag) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.flagPath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.flagPath())
}

// ClearFlag lifts the at-risk flag.
func (a *App) ClearFlag() error {
	if err := os.Remove(a.flagPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// checkFlag refuses while the stack is flagged at risk.
func (a *App) checkFlag() error {
	f, ok, err := a.Flag()
	if err != nil || !ok {
		return err
	}
	return fmt.Errorf("the PR stack is flagged at risk from %s up (%s), so nothing was pushed, landed or opened. "+
		"Run restack to rebuild it; the flag clears once the stack checks clean", f.Task, f.Cause)
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
	rr, err := trainRebase(t.Worktree, a.Cfg.Integration)
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
	if cmd := a.Cfg.Test.Cmd; cmd != NoTestCmd {
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
	if _, err := trainGit(a.Root, "update-ref", "refs/heads/"+a.Cfg.Integration, head, old); err != nil {
		// The entry is still queued, so the next land retries it.
		res.State, res.Note = store.TrainError, "integration moved during land; will retry: "+err.Error()
		return res
	}
	res.State, res.Note = store.TrainOK, head[:12]
	a.Store.Event(id, "landed", head)
	if cl, err := a.Store.Claims(); err == nil && len(cl[id]) > 0 {
		// Claims are released with the landing; keep them for the stack check.
		a.Store.Event(id, landedClaimsEvent, strings.Join(cl[id], "\n"))
	}
	if err := errors.Join(
		a.Store.SetTrain(id, store.TrainOK, old+".."+head, false),
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
// It pushes the commits the train landed, never whatever the branches point
// at, and refuses when a branch has drifted from its landed commit.
func (a *App) PRs() ([]string, error) {
	if err := a.checkFlag(); err != nil {
		return nil, err
	}
	all, err := a.landedStack()
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("nothing has landed yet")
	}
	if err := a.checkDrift(all); err != nil {
		return nil, err
	}
	if err := a.checkStack(all); err != nil {
		return nil, err
	}
	var stack []store.Task
	base := a.Cfg.Base
	for _, l := range all {
		if l.merged() {
			continue
		}
		t := l.Task
		if _, err := trainGit(a.Root, "push", "--force-with-lease=refs/heads/"+t.Branch, "origin", l.To+":refs/heads/"+t.Branch); err != nil {
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
		stack = append(stack, t)
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

// landedTask is a task the train landed, with the range of commits it landed:
// From..To on the integration branch. From is empty for entries recorded
// before the train kept ranges.
type landedTask struct {
	store.Task
	From, To string
}

// merged reports whether none of the task's commits are left on top of base,
// i.e. restack found all of them already merged.
func (l landedTask) merged() bool { return l.From != "" && l.From == l.To }

// landedStack returns the landed tasks in train order.
func (a *App) landedStack() ([]landedTask, error) {
	entries, err := a.Store.Train()
	if err != nil {
		return nil, err
	}
	var out []landedTask
	for _, e := range entries {
		if e.State != store.TrainOK {
			continue
		}
		t, err := a.Store.Task(e.Task)
		if err != nil {
			return nil, err
		}
		l := landedTask{Task: t}
		from, to, ok := strings.Cut(e.Note, "..")
		if !ok {
			to, from = e.Note, ""
		}
		if l.To, err = gitx.RevParse(a.Root, to); err != nil {
			return nil, fmt.Errorf("%s: landed commit %q is gone: %w", t.ID, to, err)
		}
		if from != "" {
			if l.From, err = gitx.RevParse(a.Root, from); err != nil {
				return nil, fmt.Errorf("%s: landed range %q is gone: %w", t.ID, e.Note, err)
			}
		}
		out = append(out, l)
	}
	return out, nil
}

// Drift lists landed tasks whose branch no longer points at the commit the
// train landed, as "task: landed X, branch Y".
func (a *App) Drift() ([]string, error) {
	stack, err := a.landedStack()
	if err != nil {
		return nil, err
	}
	return a.drift(stack), nil
}

func (a *App) drift(stack []landedTask) []string {
	var out []string
	for _, l := range stack {
		if l.merged() {
			continue
		}
		tip, err := gitx.RevParse(a.Root, "refs/heads/"+l.Branch)
		switch {
		case err != nil:
			out = append(out, fmt.Sprintf("%s: landed %s, branch %s is missing", l.ID, short(l.To), l.Branch))
		case tip != l.To:
			out = append(out, fmt.Sprintf("%s: landed %s, branch %s", l.ID, short(l.To), short(tip)))
		}
	}
	return out
}

// checkDrift refuses when any landed branch moved off its landed commit.
func (a *App) checkDrift(stack []landedTask) error {
	d := a.drift(stack)
	if len(d) == 0 {
		return nil
	}
	return fmt.Errorf("landed branches moved off the commits the train landed, so nothing was pushed. "+
		"Put each branch back on its landed commit, or land the task again:\n  %s", strings.Join(d, "\n  "))
}

// StackLayer is one landed task in the PR stack, bottom first.
type StackLayer struct {
	Task   store.Task
	Merged bool // base already has all of its work
	// Problem says why the layer's PR wouldn't show exactly its task's landed
	// work (its branch drifted, or base..head holds other work); "" if sound.
	Problem string
}

// StackLayers checks every landed task's layer of the PR stack the way prs
// does, without pushing or warning anyone.
func (a *App) StackLayers() ([]StackLayer, error) {
	stack, err := a.landedStack()
	if err != nil {
		return nil, err
	}
	var out []StackLayer
	base, baseName := a.baseRef(), a.Cfg.Base
	for _, l := range stack {
		sl := StackLayer{Task: l.Task, Merged: l.merged()}
		if !sl.Merged {
			if d := a.drift([]landedTask{l}); len(d) > 0 {
				_, sl.Problem, _ = strings.Cut(d[0], ": ")
			} else {
				probs, err := a.stackProblems(l, base, baseName)
				if err != nil {
					return nil, err
				}
				sl.Problem = strings.Join(probs, "; ")
			}
			base, baseName = l.To, l.ID+"'s branch"
		}
		out = append(out, sl)
	}
	return out, nil
}

// landedClaimsEvent records the claims a task held when it landed.
const landedClaimsEvent = "landed_claims"

// baseRef is the ref the bottom PR is diffed against: origin's copy of base
// when there is one.
func (a *App) baseRef() string {
	if sha, err := gitx.RevParse(a.Root, "refs/remotes/origin/"+a.Cfg.Base); err == nil {
		return sha
	}
	return a.Cfg.Base
}

// checkStack verifies that every PR in the stack shows exactly its task's
// work: its head descends from its base's head, base..head holds the task's
// own landed commits (same count, same patch-ids) and no merge commits. Files
// changed outside the task's claims only warn the orchestrator.
func (a *App) checkStack(stack []landedTask) error {
	var bad []string
	base, baseName := a.baseRef(), a.Cfg.Base
	for _, l := range stack {
		if l.merged() {
			continue
		}
		probs, err := a.stackProblems(l, base, baseName)
		if err != nil {
			return err
		}
		if len(probs) > 0 {
			bad = append(bad, fmt.Sprintf("%s (%s): %s", l.ID, l.Branch, strings.Join(probs, "; ")))
		} else {
			a.warnOutsideClaims(l, base)
		}
		base, baseName = l.To, l.ID+"'s branch"
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("the PR stack doesn't match what the train landed, so nothing was pushed and no PR changed. "+
		"Run restack to rebuild it; don't fix it with git:\n  %s", strings.Join(bad, "\n  "))
}

func (a *App) stackProblems(l landedTask, base, baseName string) ([]string, error) {
	var probs []string
	if mb, _ := gitx.Run(a.Root, "merge-base", base, l.To); mb != base {
		probs = append(probs, "does not descend from "+baseName)
	}
	rng := base + ".." + l.To
	merges, err := gitx.Run(a.Root, "rev-list", "--merges", rng)
	if err != nil {
		return nil, err
	}
	ids, err := patchIDs(a.Root, rng)
	if err != nil {
		return nil, err
	}
	var contains []string
	if l.From != "" {
		own, err := patchIDs(a.Root, l.From+".."+l.To)
		if err != nil {
			return nil, err
		}
		mine := map[string]bool{}
		for _, id := range own {
			mine[id] = true
		}
		in := map[string]bool{}
		foreign := 0
		for _, id := range ids {
			in[id] = true
			if !mine[id] {
				foreign++
			}
		}
		missing := 0
		for _, id := range own {
			if !in[id] {
				missing++
			}
		}
		if foreign > 0 {
			contains = append(contains, plural(foreign, "commit")+" from other tasks")
		}
		if missing > 0 {
			probs = append(probs, fmt.Sprintf("is missing %d of its %s", missing, plural(len(own), "landed commit")))
		} else if foreign == 0 && len(ids) != len(own) {
			probs = append(probs, fmt.Sprintf("has %s, landed %d", plural(len(ids), "commit"), len(own)))
		}
	}
	if merges != "" {
		contains = append(contains, plural(len(strings.Split(merges, "\n")), "merge commit"))
	}
	if len(contains) > 0 {
		probs = append(probs, "contains "+strings.Join(contains, ", "))
	}
	return probs, nil
}

// warnOutsideClaims tells the orchestrator when a PR changes files outside
// the claims its task held when it landed.
func (a *App) warnOutsideClaims(l landedTask, base string) {
	var globs []string
	evs, _ := a.Store.Events(-1)
	for _, e := range evs {
		if e.Task == l.ID && e.Kind == landedClaimsEvent {
			globs = strings.Split(e.Data, "\n")
		}
	}
	if len(globs) == 0 {
		return
	}
	files, _ := gitx.ChangedFiles(a.Root, base, l.To)
	var out []string
	for _, f := range files {
		ok := false
		for _, g := range globs {
			if claims.Match(g, f) {
				ok = true
				break
			}
		}
		if !ok {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return
	}
	msg := fmt.Sprintf("%s's PR changes files outside its claims: %s", l.ID, strings.Join(out, ", "))
	a.Store.Event(l.ID, "stack_warn", msg)
	_ = a.Notify(OrchestratorID, store.NoticeInfo, msg+".")
}

// patchIDs returns the stable patch-id of each non-merge commit in rng,
// oldest first.
func patchIDs(dir, rng string) ([]string, error) {
	log, err := gitx.Run(dir, "log", "-p", "--reverse", "--no-merges", "--no-color", "--no-ext-diff", rng)
	if err != nil {
		return nil, err
	}
	out, err := patchID(dir, log)
	if err != nil || out == "" {
		return nil, err
	}
	var ids []string
	for _, line := range strings.Split(out, "\n") {
		id, _, _ := strings.Cut(line, " ")
		ids = append(ids, id)
	}
	return ids, nil
}

// patchID runs `git patch-id --stable` over a diff or log.
func patchID(dir, diff string) (string, error) {
	cmd := exec.Command("git", "-C", dir, "patch-id", "--stable")
	cmd.Stdin = strings.NewReader(diff + "\n")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git patch-id: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// trainGit runs git as the merge train. The ref guard only lets the train
// move the integration branch and other tasks' branches, and it knows the
// train by SADDLE_TRAIN=1, set for this command alone.
func trainGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "SADDLE_TRAIN=1")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// trainRebase is gitx.Rebase run as the train: it rebases the branch checked
// out in dir onto onto and aborts on conflict, leaving the worktree as it was.
func trainRebase(dir, onto string) (gitx.RebaseResult, error) {
	out, err := trainGit(dir, "-c", "merge.directoryRenames=true", "-c", "merge.renames=true",
		"-c", "rerere.enabled=true", "-c", "core.editor=true", "rebase", onto)
	if err == nil {
		return gitx.RebaseResult{OK: true, Output: out}, nil
	}
	conf, _ := gitx.Run(dir, "diff", "--name-only", "--diff-filter=U")
	if conf == "" {
		return gitx.RebaseResult{Output: err.Error()}, err
	}
	res := gitx.RebaseResult{Conflicts: strings.Split(conf, "\n"), Output: err.Error()}
	if _, err := trainGit(dir, "rebase", "--abort"); err != nil {
		return res, err
	}
	return res, nil
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

// NoTestCmd is the [test] cmd that lands branches without running tests.
const NoTestCmd = "none"

var errNoTestCmd = errors.New(`set [test] cmd in .saddle/config.toml, or cmd = "none" to land untested`)

var makeCheckRe = regexp.MustCompile(`(?m)^check[ \t]*:([^=]|$)`)

// DetectTestCmd guesses a repo's test command from its build files: `make
// check` when the Makefile has that target, else the ecosystem's default.
func DetectTestCmd(root string) string {
	if b, err := os.ReadFile(filepath.Join(root, "Makefile")); err == nil && makeCheckRe.Match(b) {
		return "make check"
	}
	for _, c := range []struct{ file, cmd string }{
		{"go.mod", "go test ./..."},
		{"package.json", "npm test"},
		{"Cargo.toml", "cargo test"},
	} {
		if _, err := os.Stat(filepath.Join(root, c.file)); err == nil {
			return c.cmd
		}
	}
	return ""
}

// detectTestCmd writes a detected test command under [test] in
// .saddle/config.toml when no config sets one.
func (a *App) detectTestCmd() error {
	if a.Cfg.Test.Cmd != "" {
		return nil
	}
	cmd := DetectTestCmd(a.Root)
	if cmd == "" {
		return nil
	}
	path := a.stateDir("config.toml")
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s, line := string(b), fmt.Sprintf("cmd = %q\n", cmd)
	switch i := strings.Index("\n"+s, "\n[test]\n"); {
	case i >= 0:
		at := i + len("[test]\n")
		s = s[:at] + line + s[at:]
	default:
		if s != "" && !strings.HasSuffix(s, "\n") {
			s += "\n"
		}
		s += "\n[test]\n" + line
	}
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		return err
	}
	a.Cfg.Test.Cmd = cmd
	return nil
}
