package app

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// RestackMove is one ref restack moved.
type RestackMove struct {
	Task string `json:"task,omitempty"`
	Ref  string `json:"ref"`
	Old  string `json:"old"`
	New  string `json:"new"`
}

// RestackResult reports what a restack did.
type RestackResult struct {
	Base       string        `json:"base"` // the origin/<base> commit the stack now sits on
	Moves      []RestackMove `json:"moves,omitempty"`
	Merged     []string      `json:"merged,omitempty"`     // tasks whose work base already has
	Superseded []string      `json:"superseded,omitempty"` // tasks out of the stack whose commits left integration
	Dropped    int           `json:"dropped,omitempty"`    // landed commits base already has
	Retargeted []string      `json:"retargeted,omitempty"`
	// Repairing are tasks whose landed commits conflict with base while no
	// agent of theirs is alive: they left the stack for a repair task (#172).
	Repairing []string `json:"repairing,omitempty"`
	Repairs   []string `json:"repairs,omitempty"` // the repair tasks spawned
	// Backup is the ref holding integration's tip from before the restack
	// rewrote it; "" when integration only fast-forwarded or didn't move.
	Backup string `json:"backup,omitempty"`
	// GateRed are re-cut layers whose new tip fails [train] prepublish.cmd
	// (#223): prs holds them and the layers above them until they pass.
	GateRed []string `json:"gate_red,omitempty"`
}

// restacked is a landed task's place in the rebuilt stack: NewFrom..NewTo.
type restacked struct {
	landedTask
	NewFrom, NewTo string
}

func (r restacked) gone() bool { return r.NewFrom == r.NewTo }

// RestackConflict is a landed commit that no longer applies on the new base.
type RestackConflict struct {
	Task   string
	Commit string
	Files  []string
}

func (c *RestackConflict) Error() string {
	return fmt.Sprintf("restack stopped: %s's commit %s conflicts in %s; nothing was moved and %s was told",
		c.Task, short(c.Commit), strings.Join(c.Files, ", "), c.Task)
}

// Restack rebuilds the landed stack on origin/<base> after the base moved or a
// bottom PR merged. Under the train lock it first takes the tasks GitHub says
// are done out of the stack (merged, closed, killed; see ReconcileStack), then
// replays each stacked task's own landed commits in train order, dropping
// those base already has (same patch-id, or the task's files already match,
// as after a squash merge). Commits of superseded tasks are not replayed, so
// they leave integration. Only once every task replays cleanly does it move
// refs, each one compare-and-swap: the task branches, then integration. It
// then force-with-lease pushes the moved branches and retargets the open PRs,
// each onto the nearest stacked branch below it or base; a closed or merged
// PR is never retargeted. A conflict stops it before any ref moves and goes
// back to the task that owns the commit. Restack never resolves one itself.
// When the stack is flagged at risk, a successful restack re-checks it at
// once, so a flag it fixed lifts now rather than on the next sentinel cycle
// (#190).
func (a *App) Restack() (RestackResult, error) {
	res, err := a.restack()
	if err == nil {
		a.recheckFlag()
	}
	return res, err
}

// StackCheck runs one stack sentinel check. The sentinel package registers
// it, since it builds on App and can't be imported here.
var StackCheck func(*App) error

// recheckFlag re-runs the stack check when the stack is flagged. A failed
// check is recorded and left to the sentinel's next cycle.
func (a *App) recheckFlag() {
	if StackCheck == nil {
		return
	}
	if _, flagged, err := a.Flag(); err != nil || !flagged {
		return
	}
	if err := StackCheck(a); err != nil {
		a.Store.Event("", "sentinel_error", "re-check after restack: "+err.Error())
	}
}

func (a *App) restack() (RestackResult, error) {
	var res RestackResult
	unlock, err := a.lockTrain()
	if err != nil {
		return res, err
	}
	defer unlock()
	if br, _ := gitx.CurrentBranch(a.Root); br == a.Cfg.Integration {
		return res, fmt.Errorf("%s is checked out in %s; switch it to another branch so restack can move it", a.Cfg.Integration, a.Root)
	}
	remote := "refs/remotes/origin/" + a.Cfg.Base
	if _, err := trainGit(a.Root, "fetch", "origin", "+refs/heads/"+a.Cfg.Base+":"+remote); err != nil {
		return res, err
	}
	if res.Base, err = gitx.RevParse(a.Root, remote); err != nil {
		return res, err
	}
	if _, err := a.ReconcileStack(a.ghLookup); err != nil {
		return res, err
	}
	all, err := a.landedAll()
	if err != nil {
		return res, err
	}
	if len(all) == 0 {
		return res, errors.New("nothing has landed yet")
	}
	stack := stacked(all)
	for _, l := range stack {
		if l.Lost != "" {
			return res, fmt.Errorf("restack can't rebuild %s: %s. Nothing was moved", l.ID, l.Lost)
		}
	}
	a.healBranches(stack)
	if err := a.checkDrift(stack); err != nil {
		return res, err
	}
	integ, err := gitx.RevParse(a.Root, a.Cfg.Integration)
	if err != nil {
		return res, err
	}
	mb, err := gitx.Run(a.Root, "merge-base", res.Base, integ)
	if err != nil {
		return res, err
	}
	ids, err := patchIDs(a.Root, mb+".."+res.Base)
	if err != nil {
		return res, err
	}
	inBase := map[string]bool{}
	for _, id := range ids {
		inBase[id] = true
	}
	// When base already holds integration, nothing can be dropped: restack
	// just fast-forwards it.
	if mb != integ {
		if last, ok := a.lastOn(all, integ); ok {
			if err := a.checkIntegration(last, integ, res.Base, mb, inBase); err != nil {
				return res, err
			}
		}
	}
	res.Superseded = a.leaving(all, integ, inBase)

	full := stack
	plan, dropped, err := a.replay(stack, res.Base, inBase)
	// A conflict in a task with no live agent goes to a repair task instead:
	// that task leaves the replay and the rest of the stack moves on (#172).
	type orphan struct {
		l landedTask
		c *RestackConflict
	}
	var orphans []orphan
	var conflict *RestackConflict
	for errors.As(err, &conflict) && a.orphaned(conflict.Task) {
		i := slices.IndexFunc(stack, func(l landedTask) bool { return l.ID == conflict.Task })
		if i < 0 {
			break
		}
		orphans = append(orphans, orphan{stack[i], conflict})
		stack = slices.Delete(slices.Clone(stack), i, i+1)
		conflict = nil
		plan, dropped, err = a.replay(stack, res.Base, inBase)
	}
	if errors.As(err, &conflict) {
		a.returnConflict(conflict)
	}
	if err != nil {
		return res, err
	}
	res.Dropped = dropped
	for _, o := range orphans {
		a.Store.Event(o.c.Task, "restack_conflict", fmt.Sprintf("%s %s", o.c.Commit, strings.Join(o.c.Files, ", ")))
		seed := repairSeed{Source: "restack", Commit: o.c.Commit, Files: o.c.Files, Onto: "origin/" + a.Cfg.Base, Base: res.Base}
		if err := a.markRepairing(o.l, seed); err != nil {
			return res, err
		}
		res.Repairing = append(res.Repairing, o.l.ID)
	}
	// Every task still in the stack must be on the rebuilt tip before any
	// ref moves: restack never drops landed work silently (#219).
	if err := a.verifyKept(full, plan, res.Repairing, mb); err != nil {
		_ = a.Notify(OrchestratorID, store.NoticeAction, err.Error())
		return res, err
	}

	if err := a.moveStack(plan, integ, res.Base, &res); err != nil {
		return res, err
	}
	// Re-cutting changes what each tip holds (spelling words, migration
	// numbers), so the cheap checks run again on every layer that moved.
	for _, g := range a.gateRestack(plan) {
		res.GateRed = append(res.GateRed, g.Task)
	}
	if err := a.republish(plan, &res); err != nil {
		return res, err
	}
	res.Repairs = a.spawnRepairs()
	msg := fmt.Sprintf("Restacked %d landed tasks onto origin/%s at %s: %d refs moved, %d commits already in base dropped.",
		len(plan), a.Cfg.Base, short(res.Base), len(res.Moves), res.Dropped)
	if len(res.Merged) > 0 {
		msg += " Merged, so skipped: " + strings.Join(res.Merged, ", ") + "."
	}
	if len(res.Superseded) > 0 {
		msg += " Out of the stack, so their commits left " + a.Cfg.Integration + ": " + strings.Join(res.Superseded, ", ") + "."
	}
	if res.Backup != "" {
		msg += " The old " + a.Cfg.Integration + " is kept at " + res.Backup + "."
	}
	if len(res.GateRed) > 0 {
		msg += " Their new tips fail [train] prepublish.cmd, so prs holds them: " + strings.Join(res.GateRed, ", ") + "."
	}
	if len(res.Repairing) > 0 {
		msg += " Conflicting with no agent to resolve it, so out of the stack until a repair task re-lands their work: " + strings.Join(res.Repairing, ", ") + "."
	}
	if err := a.Notify(OrchestratorID, store.NoticeInfo, msg); err != nil {
		return res, err
	}
	return res, nil
}

// lastOn is the last landed task whose landed commit integration contains.
func (a *App) lastOn(all []landedTask, integ string) (landedTask, bool) {
	for i := len(all) - 1; i >= 0; i-- {
		l := all[i]
		if l.To == "" || l.Recovered {
			continue
		}
		if _, err := gitx.Run(a.Root, "merge-base", "--is-ancestor", l.To, integ); err == nil {
			return l, true
		}
	}
	return landedTask{}, false
}

// leaving lists the tasks out of the stack, not merged, whose commits are on
// integration and not in base: restack drops them.
func (a *App) leaving(all []landedTask, integ string, inBase map[string]bool) []string {
	var out []string
	for _, l := range all {
		if l.stacked() || l.State == TrainMerged || l.State == TrainRepairing || l.From == "" || l.To == "" {
			continue
		}
		if _, err := gitx.Run(a.Root, "merge-base", "--is-ancestor", l.To, integ); err != nil {
			continue
		}
		ids, _ := patchIDs(a.Root, l.From+".."+l.To)
		for _, id := range ids {
			if !inBase[id] {
				out = append(out, l.ID)
				break
			}
		}
	}
	return out
}

// checkIntegration refuses when integration holds work restack would lose:
// it must contain the last landed commit, and anything after that must
// already be in base, by SHA (base's own commits, as when integration sits on
// an older base after squash merges), by patch-id or by tree (e.g. a merge of
// base into integration).
func (a *App) checkIntegration(last landedTask, integ, base, mb string, inBase map[string]bool) error {
	if _, err := gitx.Run(a.Root, "merge-base", "--is-ancestor", last.To, integ); err != nil {
		return fmt.Errorf("%s doesn't contain %s's landed commit %s, so restack can't tell what is on it; nothing was moved",
			a.Cfg.Integration, last.ID, short(last.To))
	}
	extra, err := gitx.Run(a.Root, "rev-list", "--no-merges", "--format=%T", integ, "^"+last.To, "^"+base)
	if err != nil || extra == "" {
		return err
	}
	trees := map[string]bool{}
	if out, err := gitx.Run(a.Root, "log", "--format=%T", mb+".."+base); err == nil {
		for _, t := range strings.Fields(out) {
			trees[t] = true
		}
	}
	n := 0
	lines := strings.Split(extra, "\n")
	for i := 0; i+1 < len(lines); i += 2 {
		c := strings.TrimPrefix(lines[i], "commit ")
		if trees[lines[i+1]] {
			continue
		}
		if ids, _ := patchIDs(a.Root, c+"^!"); len(ids) == 0 || inBase[ids[0]] {
			continue // empty, or base has its change
		}
		n++
	}
	if n > 0 {
		return fmt.Errorf("%s has %s after %s's landed commit that no landed task owns and base doesn't have; restack would drop them, so nothing was moved",
			a.Cfg.Integration, plural(n, "commit"), last.ID)
	}
	return nil
}

// replay rebuilds the stack on base in a scratch worktree and returns each
// task's new range. No ref outside the scratch worktree moves.
func (a *App) replay(stack []landedTask, base string, inBase map[string]bool) ([]restacked, int, error) {
	dir := a.stateDir("restack")
	_ = os.RemoveAll(dir)
	_, _ = gitx.Run(a.Root, "worktree", "prune")
	if _, err := gitx.Run(a.Root, "worktree", "add", "--detach", dir, base); err != nil {
		return nil, 0, err
	}
	defer func() { _ = gitx.WorktreeRemove(a.Root, dir) }()

	var plan []restacked
	dropped := 0
	tip, prev := base, ""
	for _, l := range stack {
		r := restacked{landedTask: l, NewFrom: tip, NewTo: tip}
		from := l.From
		if from == "" {
			// Recorded before the train kept ranges: the task starts where the one before it ended.
			if from = prev; from == "" {
				from, _ = gitx.Run(a.Root, "merge-base", a.Cfg.Base, l.To)
			}
		}
		prev = l.To
		if a.alreadyApplied(from, l.To, tip) {
			plan = append(plan, r)
			continue
		}
		if from == tip && !a.anyInBase(from, l.To, inBase) {
			// Already on the right parent: keep the commits as they are, so
			// a restack with nothing to do moves nothing (#205).
			if _, err := gitx.Run(dir, "checkout", "-q", "--detach", l.To); err != nil {
				return nil, 0, err
			}
			r.NewFrom, r.NewTo, tip = from, l.To, l.To
			plan = append(plan, r)
			continue
		}
		commits, err := gitx.Run(a.Root, "rev-list", "--reverse", "--no-merges", from+".."+l.To)
		if err != nil {
			return nil, 0, err
		}
		for _, c := range strings.Fields(commits) {
			if ids, _ := patchIDs(a.Root, c+"^!"); len(ids) == 1 && inBase[ids[0]] {
				dropped++
				continue
			}
			empty, err := a.pick(dir, l.ID, c)
			if err != nil {
				return nil, 0, err
			}
			if empty {
				dropped++
			}
		}
		if tip, err = gitx.RevParse(dir, "HEAD"); err != nil {
			return nil, 0, err
		}
		r.NewTo = tip
		plan = append(plan, r)
	}
	return plan, dropped, nil
}

// anyInBase reports whether a commit in from..to has a change base already has.
func (a *App) anyInBase(from, to string, inBase map[string]bool) bool {
	ids, err := patchIDs(a.Root, from+".."+to)
	if err != nil {
		return true
	}
	return slices.ContainsFunc(ids, func(id string) bool { return inBase[id] })
}

// alreadyApplied reports whether every file the task changed already has the
// task's content at tip. That is how a squash merge shows up: one new commit
// on base whose patch-id matches none of the task's.
func (a *App) alreadyApplied(from, to, tip string) bool {
	files, err := gitx.ChangedFiles(a.Root, from, to)
	if err != nil || len(files) == 0 {
		return false
	}
	_, err = gitx.Run(a.Root, append([]string{"diff", "--quiet", to, tip, "--"}, files...)...)
	return err == nil
}

// pick cherry-picks c in dir. It reports a commit that became empty (base
// already has its change) and returns a *RestackConflict on conflict.
func (a *App) pick(dir, task, c string) (empty bool, err error) {
	_, err = trainGit(dir, "-c", "merge.directoryRenames=true", "-c", "merge.renames=true",
		"-c", "core.editor=true", "cherry-pick", "--allow-empty-message", c)
	if err == nil {
		return false, nil
	}
	conf, _ := gitx.Run(dir, "diff", "--name-only", "--diff-filter=U")
	if conf == "" {
		// Only a pick that left nothing to commit is empty. One that stopped
		// with changes staged (rerere replaying a resolution, say) is a
		// conflict nobody approved; skipping it would drop the commit (#219).
		_, staged := gitx.Run(dir, "diff", "--cached", "--quiet", "HEAD")
		_, unstaged := gitx.Run(dir, "diff", "--quiet")
		if _, e := gitx.RevParse(dir, "CHERRY_PICK_HEAD"); e == nil && staged == nil && unstaged == nil {
			_, err := trainGit(dir, "cherry-pick", "--skip")
			return true, err
		}
		conf, _ = gitx.Run(dir, "diff", "--name-only", "HEAD")
		_, _ = trainGit(dir, "cherry-pick", "--abort")
		if conf == "" {
			return false, err
		}
	} else {
		_, _ = trainGit(dir, "cherry-pick", "--abort")
	}
	return false, &RestackConflict{Task: task, Commit: c, Files: strings.Split(conf, "\n")}
}

// returnConflict hands a restack conflict to the task that owns the commit and
// tells the orchestrator. Saddle never resolves it.
func (a *App) returnConflict(c *RestackConflict) {
	files := strings.Join(c.Files, ", ")
	subject, _ := gitx.Run(a.Root, "log", "-1", "--format=%s", c.Commit)
	a.Store.Event(c.Task, "restack_conflict", fmt.Sprintf("%s %s", c.Commit, files))
	_ = a.Notify(c.Task, store.NoticeAction, fmt.Sprintf(
		"Restack stopped at your landed commit %s %q: it conflicts with origin/%s in %s. Nothing was moved.\n"+
			"This conflict is yours to resolve; saddle won't pick a side. Rebase your work onto origin/%s, keep both sides' intent, run the tests, and tell the orchestrator when it's ready.",
		short(c.Commit), subject, a.Cfg.Base, files, a.Cfg.Base))
	_ = a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
		"Restack stopped: %s's landed commit %s conflicts with origin/%s in %s. Nothing was moved; %s has the conflict.",
		c.Task, short(c.Commit), a.Cfg.Base, files, c.Task))
}

// moveStack moves every task branch to its rebuilt commit, then integration
// to the new tip (base when nothing is left in the stack). Each move is
// compare-and-swap and recorded in events. A task base already has all of
// leaves the stack as merged, keeping its landed range.
func (a *App) moveStack(plan []restacked, integ, base string, res *RestackResult) error {
	// Refuse before moving anything if a branch to move is checked out dirty.
	for _, r := range plan {
		if !r.gone() && r.NewTo != r.To && a.checkedOut(r.Task) {
			if dirty, _ := gitx.Dirty(r.Worktree); len(dirty) > 0 {
				return fmt.Errorf("%s's worktree %s has uncommitted changes; nothing was moved", r.ID, r.Worktree)
			}
		}
	}
	move := func(task, ref, old, new string) error {
		if _, err := trainGit(a.Root, "update-ref", ref, new, old); err != nil {
			return fmt.Errorf("%s moved during restack: %w", ref, err)
		}
		res.Moves = append(res.Moves, RestackMove{Task: task, Ref: ref, Old: old, New: new})
		a.Store.Event(task, "restack", fmt.Sprintf("%s %s → %s", ref, short(old), short(new)))
		return nil
	}
	for _, r := range plan {
		switch {
		case r.gone():
			res.Merged = append(res.Merged, r.ID)
			a.Store.Event(r.ID, "restack_merged", "base already has "+r.ID+"'s work")
			if err := a.Store.SetTrain(r.ID, TrainMerged, r.rangeNote(), false); err != nil {
				return err
			}
			continue
		case r.NewTo != r.To:
			if err := move(r.ID, "refs/heads/"+r.Branch, r.To, r.NewTo); err != nil {
				return err
			}
			if a.checkedOut(r.Task) {
				if _, err := trainGit(r.Worktree, "reset", "-q", "--hard"); err != nil {
					return err
				}
			}
		}
		if err := a.Store.SetTrain(r.ID, store.TrainOK, r.NewFrom+".."+r.NewTo, false); err != nil {
			return err
		}
	}
	tip := base
	if len(plan) > 0 {
		tip = plan[len(plan)-1].NewTo
	}
	if tip == integ {
		return nil
	}
	if _, err := gitx.Run(a.Root, "merge-base", "--is-ancestor", integ, tip); err != nil {
		// A rewrite, not a fast-forward: keep the old tip so recovery is
		// one update-ref (#219).
		backup := fmt.Sprintf("refs/saddle/integration-backups/%s-%s", time.Now().UTC().Format("20060102T150405Z"), short(integ))
		if _, err := trainGit(a.Root, "update-ref", backup, integ, ""); err != nil {
			return fmt.Errorf("saving %s's old tip before restack: %w", a.Cfg.Integration, err)
		}
		res.Backup = backup
		a.Store.Event("", "restack_backup", backup+" "+short(integ))
	}
	return move("", "refs/heads/"+a.Cfg.Integration, integ, tip)
}

// verifyKept checks the rebuilt stack against the landed one before any ref
// moves: every commit of every task that was in the stack, except those
// leaving for a repair, must be on its task's rebuilt tip, by patch-id or,
// when replaying changed the diff's context or a squash merge folded it into
// one commit, because applying the commit, or the task's whole range, there
// changes nothing. A task missing from the plan fails it outright.
func (a *App) verifyKept(stack []landedTask, plan []restacked, repairing []string, mb string) error {
	byID := map[string]restacked{}
	tip := mb
	for _, r := range plan {
		byID[r.ID] = r
		tip = r.NewTo
	}
	have, err := commitPatchIDs(a.Root, mb+".."+tip)
	if err != nil {
		return err
	}
	onTip := map[string]bool{}
	for _, id := range have {
		onTip[id] = true
	}
	var lost []string
	for _, l := range stack {
		if slices.Contains(repairing, l.ID) {
			continue
		}
		r, ok := byID[l.ID]
		if !ok {
			lost = append(lost, l.ID+" (left out of the rebuilt stack)")
			continue
		}
		if l.From == "" {
			continue // an old note without a range; replay worked it out
		}
		ids, err := commitPatchIDs(a.Root, l.From+".."+l.To)
		if err != nil {
			return err
		}
		for c, id := range ids {
			if !onTip[id] && !a.hasChange(r.NewTo, c+"^", c) && !a.hasChange(r.NewTo, l.From, l.To) {
				lost = append(lost, fmt.Sprintf("%s (commit %s)", l.ID, short(c)))
				break
			}
		}
	}
	if len(lost) == 0 {
		return nil
	}
	slices.Sort(lost)
	return fmt.Errorf("restack refused: the rebuilt %s would lose landed work of %s. Nothing was moved. "+
		"If that work is really gone from the stack, run `saddle unstack <task>` for it and restack again",
		a.Cfg.Integration, strings.Join(lost, ", "))
}

// commitPatchIDs maps each non-merge, non-empty commit in rng to its patch-id.
func commitPatchIDs(dir, rng string) (map[string]string, error) {
	log, err := gitx.Run(dir, "log", "-p", "--no-merges", "--no-color", "--no-ext-diff", rng)
	if err != nil {
		return nil, err
	}
	out, err := gitx.PatchID(dir, log)
	if err != nil {
		return nil, err
	}
	ids := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if id, c, ok := strings.Cut(line, " "); ok {
			ids[c] = id
		}
	}
	return ids, nil
}

// hasChange reports whether applying from..to on top of tip changes nothing:
// tip already has that change, as one commit or squashed with others.
func (a *App) hasChange(tip, from, to string) bool {
	tree, err := gitx.Run(a.Root, "merge-tree", "--write-tree", "--no-messages", "--merge-base="+from, tip, to)
	if err != nil {
		return false
	}
	first, _, _ := strings.Cut(tree, "\n")
	want, err := gitx.Run(a.Root, "rev-parse", tip+"^{tree}")
	return err == nil && first == want
}

// checkedOut reports whether t's worktree still exists with its branch checked out.
func (a *App) checkedOut(t store.Task) bool {
	if t.Worktree == "" {
		return false
	}
	if _, err := os.Stat(t.Worktree); err != nil {
		return false
	}
	br, err := gitx.CurrentBranch(t.Worktree)
	return err == nil && br == t.Branch
}

// republish force-with-lease pushes the moved branches that have PRs and
// retargets those PRs by the PR layout of the rebuilt stack (see stackLayout):
// each targets the layer below it in its stack, or base. A merged task's PR
// is left alone, and a PR GitHub refuses to retarget because it is closed
// takes its task out of the stack instead of failing. With stack_backend =
// "gh-stack" the stacks are linked on GitHub again afterwards.
func (a *App) republish(plan []restacked, res *RestackResult) error {
	var live []restacked
	var stack []landedTask
	for _, r := range plan {
		if r.gone() {
			continue
		}
		l := r.landedTask
		l.From, l.To = r.NewFrom, r.NewTo
		live = append(live, r)
		stack = append(stack, l)
	}
	layout, order, err := a.stackLayout(stack)
	if err != nil {
		return err
	}
	var groups []int
	var linked []store.Task
	for _, i := range order {
		r := live[i]
		if r.PR == "" {
			continue
		}
		base := a.prBase(stack, layout, i)
		head := layout[i].Head
		published, _ := gitx.RevParse(a.Root, "refs/remotes/origin/"+r.Branch)
		if r.NewTo != r.To || head != published {
			if err := a.pushLanded(r.Branch, head); err != nil {
				return err
			}
			a.Store.Event(r.ID, "restack_push", r.Branch+" "+short(head))
		}
		if _, err := gh(a.Root, "pr", "edit", r.PR, "--base", base); err != nil {
			if !strings.Contains(err.Error(), "closed pull request") {
				return err
			}
			r.From, r.To = r.NewFrom, r.NewTo
			if err := a.leaveStack(r.landedTask, TrainSuperseded, "its PR "+r.PR+" is closed"); err != nil {
				return err
			}
			continue
		}
		res.Retargeted = append(res.Retargeted, r.ID)
		a.Store.Event(r.ID, "restack_retarget", r.PR+" → "+base)
		linked = append(linked, r.Task)
		groups = append(groups, layout[i].Group)
	}
	a.linkStacks(linked, groups)
	return nil
}
