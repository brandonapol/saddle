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
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/brandonapol/saddle/internal/claims"
	"github.com/brandonapol/saddle/internal/config"
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
	// A flagged stack holds back only work that touches its broken layers.
	frozen, err := a.frozenFiles()
	if err != nil {
		return nil, err
	}
	// So does red CI: only work that would stack on a red layer (#213).
	red, repairs := a.ciRedFrozen()
	var out []LandResult
	held := 0
	for _, e := range entries {
		if e.State != store.Queued || !a.stillQueued(e.Task) {
			continue
		}
		if f := a.touches(e.Task, frozen); f != "" {
			held++
			out = append(out, LandResult{Task: e.Task, State: TrainHeld,
				Note: "held: it changes " + f + ", which the at-risk part of the stack changed; it lands once the stack checks clean"})
			continue
		}
		if f := a.touches(e.Task, red); f != "" && !repairs[e.Task] {
			held++
			out = append(out, LandResult{Task: e.Task, State: TrainHeld,
				Note: "held: it changes " + f + ", so it would stack on red CI; it lands once that layer's checks pass"})
			continue
		}
		r := a.landOne(e.Task)
		out = append(out, r)
	}
	if held > 0 && held == len(out) {
		return out, a.checkFlag()
	}
	return out, nil
}

// stillQueued reports whether task's entry is still queued: a hold or move
// made while the train runs takes effect on the entries it hasn't reached.
func (a *App) stillQueued(task string) bool {
	es, err := a.Store.Train()
	if err != nil {
		return true
	}
	for _, e := range es {
		if e.Task == task {
			return e.State == store.Queued
		}
	}
	return false
}

// frozenFiles maps each file the at-risk layers of a flagged stack changed to
// the layer; nil when the stack isn't frozen.
func (a *App) frozenFiles() (map[string]string, error) {
	f, ok, err := a.Flag()
	if err != nil || !ok || f.Acked {
		return nil, err
	}
	stack, err := a.landedStack()
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	at := false
	for _, l := range stack {
		at = at || l.ID == f.Task
		if !at || l.From == "" || l.Lost != "" {
			continue
		}
		files, _ := gitx.ChangedFiles(a.Root, l.From, l.To)
		for _, file := range files {
			out[file] = l.ID
		}
	}
	return out, nil
}

// touches names a file the queued task changed that frozen holds, or "".
func (a *App) touches(task string, frozen map[string]string) string {
	if len(frozen) == 0 {
		return ""
	}
	t, err := a.Store.Task(task)
	if err != nil {
		return ""
	}
	out, err := gitx.Run(a.Root, "diff", "--name-only", a.Cfg.Integration+"..."+t.Branch)
	if err != nil || out == "" {
		return ""
	}
	for _, f := range strings.Split(out, "\n") {
		if owner, ok := frozen[f]; ok {
			return f + " (" + owner + ")"
		}
	}
	return ""
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
	// Acked means someone acknowledged the flag (sentinel ack): it freezes
	// nothing and carries no labels until the first broken task changes.
	Acked bool `json:"acked,omitempty"`
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

// checkFlag refuses while the stack is flagged at risk and not acked.
func (a *App) checkFlag() error {
	f, ok, err := a.Flag()
	if err != nil || !ok || f.Acked {
		return err
	}
	return flagErr(f)
}

func flagErr(f StackFlag) error {
	return fmt.Errorf("the PR stack is flagged at risk from %s up (%s), so nothing from there up was pushed, landed or opened. "+
		"Run restack to rebuild it; the flag clears once the stack checks clean. "+
		"If it can't be fixed that way, `saddle unstack %s` drops the task from the stack and `saddle sentinel ack` acknowledges the flag",
		f.Task, f.Cause, f.Task)
}

func (a *App) landOne(id string) LandResult {
	res := LandResult{Task: id}
	fail := func(state, note, msg string) LandResult {
		res.State, res.Note = state, note
		a.Store.Event(id, "train_"+state, note)
		if err := a.Store.SetTrain(id, state, note, true); err != nil {
			res.Note += " (could not record: " + err.Error() + ")"
		}
		if n := a.attempts(id); n >= a.Cfg.Train.MaxAttempts {
			return a.escalate(res, n, state, msg)
		}
		if err := errors.Join(
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
	from, _, err := a.replayFrom(t, a.Cfg.Integration)
	if err != nil {
		return fail(store.TrainError, "rebase failed", "Finding your branch's own commits failed:\n"+err.Error()+"\nFix it and call done again.")
	}
	rr, taken, err := trainRebase(t.Worktree, a.Cfg.Integration, from, a.Cfg.Regen)
	if err != nil {
		return fail(store.TrainError, "rebase failed", "Rebasing your branch onto "+a.Cfg.Integration+" failed:\n"+rr.Output+"\nFix it and call done again.")
	}
	if !rr.OK {
		files := strings.Join(rr.Conflicts, ", ")
		if a.orphaned(id) {
			// Nobody would read the conflict: a repair task takes it (#172).
			res.State, res.Note = store.TrainError, files
			a.Store.Event(id, "train_"+store.TrainError, files)
			if err := errors.Join(a.Store.SetTrain(id, store.TrainError, files, true), a.Store.SetStatus(id, store.Conflict)); err != nil {
				res.Note += " (could not record: " + err.Error() + ")"
			}
			seed := a.landSeed(t, files)
			seed.From = from
			if r, _, err := a.ensureRepair(t, seed, false); err == nil {
				res.Note += "; its agent is gone, so " + r.ID + " repairs it"
			}
			return res
		}
		return fail(store.TrainError, files, fmt.Sprintf(
			"Your branch could not land: rebasing onto %s conflicts in %s. Other work already landed there, so this conflict is yours to fix.\n"+
				"1. Run `saddle sync`. It starts the rebase and stops at the conflicts.\n"+
				"2. Resolve them, keeping both sides' intent, then `git add` and `git rebase --continue`.\n"+
				"3. Run the tests, then call the saddle done tool again.", a.Cfg.Integration, files))
	}
	regenerated := ""
	if len(taken) > 0 {
		if out, err := regenerate(t.Worktree, a.Cfg.Regen, taken); err != nil {
			// The branch stays rebased, with the failed output in the worktree to look at.
			return fail(store.TrainError, "regen failed", fmt.Sprintf(
				"Your branch rebased onto %s, keeping %s's copy of the derived files %s, but regenerating them failed:\n%s\n%s\nFix it, commit, and call the saddle done tool again.",
				a.Cfg.Integration, a.Cfg.Integration, strings.Join(taken, ", "), out, err))
		}
		regenerated = " (regenerated " + strings.Join(taken, ", ") + ")"
		a.Store.Event(id, "train_regen", strings.Join(taken, "\n"))
	}
	if cmd := a.Cfg.Test.Cmd; cmd != NoTestCmd {
		if out, err := runShell(t.Worktree, cmd); err != nil {
			return fail(store.TestFailed, "tests failed", fmt.Sprintf(
				"Your branch rebased cleanly onto %s, but `%s` failed on the result:\n%s\nFix it, commit, and call the saddle done tool again.", a.Cfg.Integration, cmd, tail(out, 40)))
		}
	}
	if msg := a.trainLint(t.Worktree); msg != "" {
		return fail(store.TestFailed, "lint failed", msg)
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
	res.State, res.Note = store.TrainOK, head[:12]+regenerated
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
	if err := a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf("%s %q landed on %s at %s.%s", id, t.Title, a.Cfg.Integration, head[:12], a.repairLanded(id))); err != nil {
		res.Note += " (orchestrator not notified: " + err.Error() + ")"
	}
	return res
}

// attempts is how many times id has failed to land.
func (a *App) attempts(id string) int {
	es, err := a.Store.Train()
	if err != nil {
		return 0
	}
	for _, e := range es {
		if e.Task == id {
			return e.Attempts
		}
	}
	return 0
}

// escalate hands a branch that failed to land n times to the owner instead of
// its producer: its train entry becomes escalated, the task needs-you, and
// the orchestrator gets the failure. The producer hears about it without
// being woken, so it doesn't loop on the same failure (#30).
func (a *App) escalate(res LandResult, n int, state, msg string) LandResult {
	why := res.Note
	res.State = TrainEscalated
	res.Note = fmt.Sprintf("%s; escalated after %s", why, plural(n, "failed attempt"))
	a.Store.Event(res.Task, "train_escalated", res.Note)
	t, _ := a.Store.Task(res.Task)
	if err := errors.Join(
		a.Store.SetTrain(res.Task, TrainEscalated, why+" ("+state+")", false),
		a.Store.SetStatus(res.Task, store.NeedsYou),
		a.Notify(res.Task, store.NoticeInfo, fmt.Sprintf(
			"Your branch failed to land %s times, so the train escalated it to the owner instead of handing it back again. "+
				"Stop retrying; wait for instructions. The last failure:\n%s", strconv.Itoa(n), msg)),
		a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
			"▲ %s %q needs you: it failed to land %s (last: %s: %s), so the train stopped returning it. "+
				"Fix it yourself, spawn a repair task, or tell %s what to do; done queues it again.\nLast failure:\n%s",
			res.Task, t.Title, plural(n, "time"), state, why, res.Task, msg)),
	); err != nil {
		res.Note += " (could not record or notify: " + err.Error() + ")"
	}
	return res
}

// broadcastLanding records renames, remaps the other tasks' claims through
// them, rebases every live agent's clean worktree onto the new head, and
// tells each agent what moved under it.
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
		shared := overlaps(t.Worktree, old, changed)
		switch rb := a.autoRebase(t); {
		case rb.To != "":
			fmt.Fprintf(&msg, "\nSaddle rebased your branch onto it (%s → %s), following directory moves. Files may have changed under you: re-read them before editing.", short(rb.From), short(rb.To))
		case len(rb.Conflicts) > 0:
			kind = store.NoticeAction
			fmt.Fprintf(&msg, "\nRebasing your branch onto it conflicts in %s, so saddle left your branch as it was. At your next clean point run `saddle sync` and resolve them, keeping both sides' intent.", strings.Join(rb.Conflicts, ", "))
		default:
			if shared {
				kind = store.NoticeAction
				msg.WriteString("\nIt touched files you changed too, so sync now to avoid a conflict later.")
			}
			msg.WriteString("\nAt your next clean point (commit first), run `saddle sync` to rebase onto it. Directory moves are followed automatically.")
		}
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

// PRs pushes every landed branch in the stack and opens or updates its PR,
// laid out as stacks (see stackLayout): dependent or same-topic tasks stack,
// each PR on the one below it, and unrelated tasks' PRs target base (#52);
// the owner's custom stacks stack in the order given (#211). A layer
// on its train predecessor pushes the commit the train landed, never whatever
// its branch points at; any other pushes those landed commits replayed onto
// the PR below. Tasks GitHub says are done leave the stack first (see
// ReconcileStack). Layers below the first broken or flagged one are
// published; from there up nothing is pushed or changed, and the error says
// why.
func (a *App) PRs() ([]string, error) {
	res, err := a.publish()
	return res.urls, err
}

// published is what one prs did.
type published struct {
	urls  []string
	links []stackLink // gh-stack links, with stack_backend = "gh-stack"
}

// publish is PRs. Layers go out in the layout's order (see stackLayout), so
// a PR's base branch is pushed before the PR targets it.
func (a *App) publish() (published, error) {
	var res published
	unlock, err := a.lockTrain()
	if err != nil {
		return res, err
	}
	defer unlock()
	if _, err := a.ReconcileStack(a.ghLookup); err != nil {
		return res, err
	}
	all, err := a.landedAll()
	if err != nil {
		return res, err
	}
	if len(all) == 0 {
		return res, fmt.Errorf("nothing has landed yet")
	}
	landed := stacked(all)
	a.healBranches(landed)
	flag, flagged, err := a.Flag()
	if err != nil {
		return res, err
	}
	layout, order, err := a.stackLayout(landed)
	if err != nil {
		return res, err
	}
	var stop error
	stopAt := len(landed)
	ciHeld, ciSkipped := a.ciRedHeld(landed, layout), map[int]string{}
	base, baseName := a.baseRef(), a.Cfg.Base
	for i, l := range landed {
		if flagged && !flag.Acked && l.ID == flag.Task {
			stop, stopAt = flagErr(flag), i
			break
		}
		prob, err := a.layerProblem(l, base, baseName)
		if err != nil {
			return res, err
		}
		if prob != "" {
			stop = fmt.Errorf("the PR stack doesn't match what the train landed, so nothing from %s up was pushed and no PR there changed. "+
				"Run restack to rebuild it; don't fix it with git:\n  %s: %s", l.ID, l.ID, prob)
			stopAt = i
			break
		}
		a.warnOutsideClaims(l, base)
		base, baseName = l.To, l.ID+"'s branch"
	}
	var stack []store.Task
	var groups []int
	done := map[int]bool{}
	for _, i := range order {
		if i >= stopAt {
			continue
		}
		if r, ok := ciHeld[i]; ok {
			ciSkipped[i] = r // CI is red below it (#213)
			continue
		}
		if b := layout[i].Below; b >= 0 && !done[b] {
			continue // the PR below it wasn't published
		}
		l := landed[i]
		t := l.Task
		if err := a.pushLanded(t.Branch, layout[i].Head); err != nil {
			return res, err
		}
		if layout[i].Head != l.To {
			a.Store.Event(t.ID, "pr_layout", fmt.Sprintf("%s published as %s on %s", short(l.To), short(layout[i].Head), a.prBase(landed, layout, i)))
		}
		prBase := a.prBase(landed, layout, i)
		if t.PR == "" {
			url, err := gh(a.Root, "pr", "create", "--base", prBase, "--head", t.Branch, "--title", t.Title, "--body", t.Summary)
			if err != nil {
				return res, err
			}
			t.PR = lastLine(url)
			if err := a.Store.SetField(t.ID, "pr", t.PR); err != nil {
				return res, err
			}
		} else if _, err := gh(a.Root, "pr", "edit", t.PR, "--base", prBase); err != nil {
			return res, err
		}
		done[i] = true
		stack = append(stack, t)
		groups = append(groups, layout[i].Group)
	}
	// stack is in layout order, so each group's tasks are bottom first.
	for i, t := range stack {
		var b strings.Builder
		b.WriteString(t.Summary)
		if t.Issue > 0 {
			fmt.Fprintf(&b, "\n\nCloses #%d", t.Issue)
		}
		var mine []store.Task
		for j, u := range stack {
			if groups[j] == groups[i] {
				mine = append(mine, u)
			}
		}
		if len(mine) > 1 {
			b.WriteString("\n\n---\n**Stack** (opened by saddle; merge bottom-up)\n\n")
			for j := len(mine) - 1; j >= 0; j-- {
				mark := ""
				if mine[j].ID == t.ID {
					mark = " 👈"
				}
				fmt.Fprintf(&b, "%d. %s %s%s\n", j+1, mine[j].PR, mine[j].Title, mark)
			}
		} else {
			b.WriteString("\n\n---\nOpened by saddle; it doesn't depend on another saddle PR.\n")
		}
		b.WriteString("\nBase: `" + a.Cfg.Base + "`\n")
		if _, err := gh(a.Root, "pr", "edit", t.PR, "--body", b.String()); err != nil {
			return res, err
		}
		res.urls = append(res.urls, t.PR)
	}
	res.links = a.linkStacks(stack, groups)
	if stop == nil {
		stop = ciRedErr(ciSkipped, landed)
	}
	return res, stop
}

// prBase is the branch layer i's PR targets: base, or the branch of the
// layer below it in its stack.
func (a *App) prBase(stack []landedTask, layout []prLayer, i int) string {
	if b := layout[i].Below; b >= 0 {
		return stack[b].Branch
	}
	return a.Cfg.Base
}

// landedTask is a task the train landed, with the range of commits it landed:
// From..To on the integration branch, and the train state it is in.
type landedTask struct {
	store.Task
	From, To string
	State    string
	// Recovered marks a range rebuilt from the task's landing because its
	// train note recorded none (notes written before the train kept ranges, or
	// emptied by an old restack, #123).
	Recovered bool
	// Lost says why the task's landed work can't be told apart; "" if it can.
	Lost string
}

// stacked reports whether the task is part of the PR stack: it landed, and
// nothing (a merge, a close, a kill, unstack) has taken it out since.
func (l landedTask) stacked() bool { return l.State == store.TrainOK && l.Status != store.Killed }

// rangeNote is the train note for l's landed range.
func (l landedTask) rangeNote() string {
	if l.From == "" {
		return l.To
	}
	return l.From + ".." + l.To
}

func stacked(all []landedTask) []landedTask {
	var out []landedTask
	for _, l := range all {
		if l.stacked() {
			out = append(out, l)
		}
	}
	return out
}

// landedStack returns the tasks in the PR stack in train order.
func (a *App) landedStack() ([]landedTask, error) {
	all, err := a.landedAll()
	if err != nil {
		return nil, err
	}
	return stacked(all), nil
}

// landedAll returns every task the train landed, in train order, whether or
// not it is still in the stack. It resolves the landed ranges in one git
// process, however many tasks have landed. A note without a usable range is
// rebuilt from the task's landing event; one that can't be is marked Lost
// rather than read as "merged".
func (a *App) landedAll() ([]landedTask, error) {
	entries, err := a.Store.Train()
	if err != nil {
		return nil, err
	}
	tasks, err := a.Store.Tasks()
	if err != nil {
		return nil, err
	}
	byID := make(map[string]store.Task, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}
	type span struct {
		landedTask
		note, from, to string
		ranged         bool
	}
	var spans []span
	var refs []string
	for _, e := range entries {
		if e.State != store.TrainOK && e.State != TrainMerged && e.State != TrainSuperseded && e.State != TrainRepairing {
			continue
		}
		t, ok := byID[e.Task]
		if !ok || e.Note == "" {
			continue
		}
		from, to, ranged := strings.Cut(e.Note, "..")
		if !ranged {
			to, from = e.Note, ""
		}
		spans = append(spans, span{landedTask{Task: t, State: e.State}, e.Note, from, to, ranged})
		refs = append(refs, to)
		if from != "" {
			refs = append(refs, from)
		}
	}
	commits, err := gitx.ResolveCommits(a.Root, refs)
	if err != nil {
		return nil, err
	}
	var landings []store.Event // read only if a note needs it
	readLandings := func() []store.Event {
		if landings == nil {
			evs, _ := a.Store.Events(-1)
			landings = []store.Event{}
			for _, e := range evs {
				if e.Kind == "landed" {
					landings = append(landings, e)
				}
			}
		}
		return landings
	}
	out := make([]landedTask, 0, len(spans))
	prev := ""
	for _, sp := range spans {
		l := sp.landedTask
		l.To = commits[sp.to]
		if sp.from != "" {
			l.From = commits[sp.from]
		}
		if !sp.ranged || l.From == "" || l.From == l.To {
			// No usable range: take it from the task's landing if one says.
			if from, to, ok := a.landingRange(l.ID, readLandings()); ok {
				l.From, l.To, l.Recovered = from, to, true
			} else if !sp.ranged && l.To != "" {
				if l.From = prev; l.From == "" || l.From == l.To {
					l.From, _ = gitx.Run(a.Root, "merge-base", a.Cfg.Base, l.To)
				}
			}
		}
		switch {
		case l.To == "":
			l.Lost = fmt.Sprintf("its landed commit %q is gone", sp.to)
		case l.From == "" || l.From == l.To:
			l.From = ""
			l.Lost = fmt.Sprintf("its train note %q records none of its commits and no landing says which", sp.note)
		}
		if l.Lost != "" && l.stacked() {
			l.Lost += fmt.Sprintf("; run `saddle requeue %s` to land it again, or `saddle unstack %s` if its work is elsewhere", l.ID, l.ID)
		}
		if l.To != "" {
			prev = l.To
		}
		out = append(out, l)
	}
	return out, nil
}

// landingRange is the range task's last landing put on integration, from the
// landing events: from the head the landing before it left to its own head.
func (a *App) landingRange(task string, landings []store.Event) (from, to string, ok bool) {
	at := -1
	for i, e := range landings {
		if e.Task == task {
			at = i
		}
	}
	if at < 0 {
		return "", "", false
	}
	to, err := gitx.RevParse(a.Root, landings[at].Data)
	if err != nil {
		return "", "", false
	}
	for i := at - 1; i >= 0 && from == ""; i-- {
		if landings[i].Task != task {
			from, _ = gitx.RevParse(a.Root, landings[i].Data)
		}
	}
	if from == "" {
		from, _ = gitx.Run(a.Root, "merge-base", a.Cfg.Base, to)
	}
	if from == "" || from == to {
		return "", "", false
	}
	if _, err := gitx.Run(a.Root, "merge-base", "--is-ancestor", from, to); err != nil {
		return "", "", false
	}
	return from, to, true
}

// Drift lists stacked tasks whose branch no longer points at the commit the
// train landed, as "task: landed X, branch Y".
func (a *App) Drift() ([]string, error) {
	stack, err := a.landedStack()
	if err != nil {
		return nil, err
	}
	return a.drift(stack), nil
}

func (a *App) drift(stack []landedTask) []string {
	var refs []string
	for _, l := range stack {
		refs = append(refs, "refs/heads/"+l.Branch)
	}
	tips, err := gitx.ResolveCommits(a.Root, refs)
	if err != nil {
		return []string{"can't read landed branches: " + err.Error()}
	}
	var out []string
	for _, l := range stack {
		if l.Lost != "" || l.Recovered {
			continue // nothing reliable to compare with
		}
		tip, ok := tips["refs/heads/"+l.Branch]
		switch {
		case !ok:
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

// StackLayer is one task in the PR stack, bottom first.
type StackLayer struct {
	Task store.Task
	// Problem says why the layer's PR wouldn't show exactly its task's landed
	// work (its branch drifted, base..head holds other work, its work isn't
	// on integration); "" if sound.
	Problem string
}

// StackLayers checks every layer of the PR stack the way prs does, without
// pushing or warning anyone. Deleted local branches are restored first.
func (a *App) StackLayers() ([]StackLayer, error) {
	stack, err := a.landedStack()
	if err != nil {
		return nil, err
	}
	a.healBranches(stack)
	var out []StackLayer
	base, baseName := a.baseRef(), a.Cfg.Base
	for _, l := range stack {
		prob, err := a.layerProblem(l, base, baseName)
		if err != nil {
			return nil, err
		}
		out = append(out, StackLayer{Task: l.Task, Problem: prob})
		if l.Lost == "" {
			base, baseName = l.To, l.ID+"'s branch"
		}
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

// layerProblem says why l's PR, on top of base, wouldn't show exactly its
// task's work: its range is lost, its branch drifted, its recovered work
// isn't on integration, or base..head holds other work or merges. Files
// changed outside the task's claims are not a problem; prs only warns.
func (a *App) layerProblem(l landedTask, base, baseName string) (string, error) {
	if l.Lost != "" {
		return l.Lost, nil
	}
	if d := a.drift([]landedTask{l}); len(d) > 0 {
		_, p, _ := strings.Cut(d[0], ": ")
		return p, nil
	}
	if l.Recovered {
		if _, err := gitx.Run(a.Root, "merge-base", "--is-ancestor", l.To, a.Cfg.Integration); err != nil {
			return fmt.Sprintf("its landed work (%s) is not on %s; restack puts it back", short(l.To), a.Cfg.Integration), nil
		}
	}
	probs, err := a.stackProblems(l, base, baseName)
	return strings.Join(probs, "; "), err
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
func patchIDs(dir, rng string) ([]string, error) { return gitx.PatchIDs(dir, rng) }

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
// With from set it replays only from..HEAD, as `rebase --onto onto from`.
// Conflicts only in derived files regen covers are not merged: the rebase
// takes onto's copy and goes on, and the files are returned in regen so the
// caller can regenerate them (#28).
func trainRebase(dir, onto, from string, regen []config.Regen) (res gitx.RebaseResult, taken []string, err error) {
	args := []string{"-c", "merge.directoryRenames=true", "-c", "merge.renames=true",
		"-c", "rerere.enabled=true", "-c", "core.editor=true", "rebase"}
	if from != "" {
		args = append(args, "--onto")
	}
	args = append(args, onto)
	if from != "" {
		args = append(args, from)
	}
	out, err := trainGit(dir, args...)
	for err != nil {
		conf, _ := gitx.Run(dir, "diff", "--name-only", "--diff-filter=U")
		if conf == "" {
			if gitx.RebaseInProgress(dir) {
				_, _ = trainGit(dir, "rebase", "--abort")
			}
			return gitx.RebaseResult{Output: err.Error()}, nil, err
		}
		files := strings.Split(conf, "\n")
		if !allRegen(regen, files) {
			res := gitx.RebaseResult{Conflicts: files, Output: err.Error()}
			if _, err := trainGit(dir, "rebase", "--abort"); err != nil {
				return res, nil, err
			}
			return res, nil, nil
		}
		for _, f := range files {
			if !slices.Contains(taken, f) {
				taken = append(taken, f)
			}
		}
		// During a rebase "ours" is onto plus the commits replayed so far.
		if _, err := trainGit(dir, append([]string{"checkout", "--ours", "--"}, files...)...); err != nil {
			_, _ = trainGit(dir, "rebase", "--abort")
			return gitx.RebaseResult{Output: err.Error()}, nil, err
		}
		if _, err := trainGit(dir, append([]string{"add", "--"}, files...)...); err != nil {
			_, _ = trainGit(dir, "rebase", "--abort")
			return gitx.RebaseResult{Output: err.Error()}, nil, err
		}
		step := "--continue"
		if _, e := gitx.Run(dir, "diff", "--cached", "--quiet", "HEAD"); e == nil {
			step = "--skip" // the commit only changed derived files
		}
		out, err = trainGit(dir, "-c", "core.editor=true", "-c", "rerere.enabled=true", "rebase", step)
	}
	return gitx.RebaseResult{OK: true, Output: out}, taken, nil
}

// allRegen reports whether regen covers every one of files.
func allRegen(regen []config.Regen, files []string) bool {
	if len(regen) == 0 {
		return false
	}
	for _, f := range files {
		if !regenMatch(regen, f) {
			return false
		}
	}
	return true
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
