package app

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// A red layer gets one repair at a time, once per red head (#213): a sonnet
// task that fixes the failure on the red layer itself. Its branch lands
// through the train like any other, and then FoldCIRepairs moves its
// commits right after the red layer's and makes them part of that layer, so
// they go out on the red PR's branch instead of as a new PR above it (#196).
// A lint or format failure first gets the repo's own fixer (make fix, or
// what lintgate detects), whose commit the repair starts from. After
// [ci] repair_attempts repairs the layer is still red, the orchestrator is
// asked instead. A failure a pending sibling PR explains is no repair case
// (#193): it clears once that PR merges and the stack restacks.

// TrainFolded is the train state of a CI repair whose commits joined the
// red layer it repaired: out of the stack, with no PR of its own.
const TrainFolded = "folded"

// CI repair events, on the red task.
const (
	eventCIRepairSpawned = "ci_repair_spawned" // data: repair task id
	eventCIRepairFailed  = "ci_repair_failed"  // data: why it couldn't be spawned
	eventCIRepairFolded  = "ci_repair_folded"  // data: repair task id
	eventCIRepairFixer   = "ci_repair_fixer"   // data: fixer command and commit
	eventCIRedExplained  = "ci_red_explained"  // data: the sibling that explains it
	eventCIRedEscalated  = "ci_red_escalated"
)

// CIRedFailure is one failing check on a red head, with its log tail.
type CIRedFailure struct {
	Check   string
	RunURL  string
	Step    string
	LogTail string
}

// CIRedExplained reports a pending sibling whose unmerged work explains the
// failures of task's red head (#193). The dependency work of #193 may swap
// in a sharper test; the default is siblingExplains.
var CIRedExplained = func(a *App, task string, fails []CIRedFailure) (string, bool) {
	return a.siblingExplains(task, fails)
}

// CIRedRepair decides what to do about task's red head and does it: nothing
// while a repair is live or one was spawned for this head, nothing when a
// sibling explains it, escalate once the attempts are spent, else run the
// fixer for a lint failure and spawn the repair. It returns a sentence for
// the orchestrator's notice.
func (a *App) CIRedRepair(task string, fails []CIRedFailure) (string, error) {
	s, err := a.CIRed()
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(s.Red, func(l CIRedLayer) bool { return l.Task == task })
	if i < 0 {
		return "", nil
	}
	l := &s.Red[i]
	owner, err := a.Store.Task(task)
	if err != nil {
		return "", err
	}
	save := func(note string) (string, error) { return note, a.SetCIRed(s) }
	if r, ok := a.liveCIRepair(s, task); ok {
		l.Repair = r
		return save(r + " is already repairing it.")
	}
	if owner.Active() {
		return owner.ID + "'s agent is still live, so the CI watcher hands the failure to it.", nil
	}
	if fix, ok := a.ciFixTask(owner); ok {
		// ciwatch spawned one under the same title first: adopt it.
		s.Repairs = append(s.Repairs, CIRepairRecord{Task: task, Head: l.Head, Repair: fix.ID, State: ciRepairSpawned})
		l.Repair = fix.ID
		a.Store.Event(task, eventCIRepairSpawned, fix.ID)
		return save(fix.ID + " is repairing it on " + task + "'s layer.")
	}
	if slices.ContainsFunc(s.Repairs, func(r CIRepairRecord) bool { return r.Task == task && r.Head == l.Head }) {
		return "", nil // one repair per red head
	}
	key := task + "@" + l.Head
	if sib, ok := CIRedExplained(a, task, fails); ok {
		if slices.Contains(s.Explained, key) {
			return "", nil
		}
		s.Explained = append(s.Explained, key)
		a.Store.Event(task, eventCIRedExplained, sib)
		return save(fmt.Sprintf("The failure looks explained by %s's work, which isn't merged yet (#193), so no repair was spawned: it should clear once %s merges and the stack restacks.", sib, sib))
	}
	n := 0
	for _, r := range s.Repairs {
		if r.Task == task {
			n++
		}
	}
	if n >= a.Cfg.CI.RepairAttempts {
		if l.Escalated {
			return "", nil
		}
		l.Escalated = true
		a.Store.Event(task, eventCIRedEscalated, fmt.Sprintf("%d repairs", n))
		note, err := save(fmt.Sprintf("%d repairs ran and it is still red, so none more is spawned.", n))
		return note, errors.Join(err, a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
			"CI on %s's PR %s is still red after %d repair attempts (%s at %s). Saddle won't spawn another. "+
				"Decide what to do: fix it on %s's layer, `saddle unstack %s` to drop it, or `saddle sentinel ack` to stop it holding the layers above (%s).",
			task, l.PR, n, strings.Join(l.Checks, ", "), short(l.Head), task, task, strings.Join(l.Held, ", "))))
	}

	fixer, fixCommit := "", ""
	if isLintFailure(fails) {
		fixer, fixCommit = a.ciRunFixer(owner, task)
	}
	req := SpawnReq{
		Title:   ciFixTitle(owner),
		Prompt:  a.ciRepairPrompt(owner, *l, fails, fixer, fixCommit, n+1),
		Model:   "sonnet",
		Parent:  OrchestratorID,
		Issue:   owner.Issue,
		Claims:  a.layerFiles(task),
		Confirm: true,
	}
	r, err := a.Spawn(req)
	if err != nil && len(req.Claims) > 0 {
		// Live work may hold the layer's files by now; the repair can claim as it goes.
		req.Claims = nil
		r, err = a.Spawn(req)
	}
	if err != nil {
		why := err.Error()
		if last, ok := a.lastEvent(task, eventCIRepairFailed); !ok || last != why {
			a.Store.Event(task, eventCIRepairFailed, why)
			_ = a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
				"CI is red on %s's PR %s and saddle couldn't spawn its repair: %s\nIt retries on the next ci-red cycle; spawn one titled %q yourself if that keeps failing.",
				task, l.PR, why, req.Title))
		}
		return "", err
	}
	s.Repairs = append(s.Repairs, CIRepairRecord{Task: task, Head: l.Head, Repair: r.ID, Fixer: fixCommit, State: ciRepairSpawned})
	l.Repair = r.ID
	a.Store.Event(task, eventCIRepairSpawned, r.ID)
	note := fmt.Sprintf("Spawned %s (sonnet, attempt %d of %d) to fix it on %s's layer; its commits join %s's PR, never a new one.",
		r.ID, n+1, a.Cfg.CI.RepairAttempts, task, task)
	if fixCommit != "" {
		note += fmt.Sprintf(" `%s` already fixed some of it (%s).", fixer, short(fixCommit))
	}
	return save(note)
}

// liveCIRepair is the repair of task still working or waiting to be folded.
func (a *App) liveCIRepair(s CIRedState, task string) (string, bool) {
	for _, r := range s.Repairs {
		if r.Task != task || r.State != ciRepairSpawned {
			continue
		}
		if t, err := a.Store.Task(r.Repair); err == nil && t.Status != store.Killed {
			return r.Repair, true
		}
	}
	return "", false
}

// layerFiles lists the files task's landed layer changed.
func (a *App) layerFiles(task string) []string {
	stack, err := a.landedStack()
	if err != nil {
		return nil
	}
	for _, l := range stack {
		if l.ID == task && l.From != "" && l.Lost == "" {
			files, _ := gitx.ChangedFiles(a.Root, l.From, l.To)
			return files
		}
	}
	return nil
}

var lintCheckRe = regexp.MustCompile(`(?i)lint|fmt|format|vet|spell|prettier|eslint|style|structure`)
var lintLogRe = regexp.MustCompile(`(?i)gofmt|golangci|dart format|cspell|prettier|eslint|needs formatting|would reformat|not formatted`)

// isLintFailure reports a lint or format failure: the first-class case,
// which the repo's own fixer may fix outright.
func isLintFailure(fails []CIRedFailure) bool {
	for _, f := range fails {
		if lintCheckRe.MatchString(f.Check) || lintCheckRe.MatchString(f.Step) || lintLogRe.MatchString(f.LogTail) {
			return true
		}
	}
	return false
}

// ciFixRef keeps the fixer's commit for task's repair to start from.
func ciFixRef(task string) string { return "refs/saddle/ci-fix/" + task }

// ciRunFixer runs the repo's fixer on task's landed layer in a scratch
// worktree and commits what it changed. It returns the fixer and the commit,
// "" when there is no fixer or it changed nothing.
func (a *App) ciRunFixer(owner store.Task, task string) (fixer, commit string) {
	fixer = a.LintGate().Fix
	if fixer == "" {
		return "", ""
	}
	stack, err := a.landedStack()
	if err != nil {
		return fixer, ""
	}
	i := slices.IndexFunc(stack, func(l landedTask) bool { return l.ID == task })
	if i < 0 {
		return fixer, ""
	}
	dir := a.stateDir("ci-fix")
	_ = os.RemoveAll(dir)
	_, _ = gitx.Run(a.Root, "worktree", "prune")
	if _, err := gitx.Run(a.Root, "worktree", "add", "--detach", dir, stack[i].To); err != nil {
		return fixer, ""
	}
	defer func() { _ = gitx.WorktreeRemove(a.Root, dir) }()
	out, err := runShell(dir, fixer)
	if err != nil {
		a.Store.Event(task, eventCIRepairFixer, fmt.Sprintf("%s failed: %s", fixer, tail(out, 5)))
		return fixer, ""
	}
	if st, _ := gitx.Run(dir, "status", "--porcelain"); st == "" {
		a.Store.Event(task, eventCIRepairFixer, fixer+" changed nothing")
		return fixer, ""
	}
	if _, err := gitx.Run(dir, "add", "-A"); err != nil {
		return fixer, ""
	}
	if _, err := trainGit(dir, "commit", "-q", "--no-verify", "-m", fmt.Sprintf("Run %s for %s's red CI", fixer, owner.ID)); err != nil {
		return fixer, ""
	}
	head, err := gitx.RevParse(dir, "HEAD")
	if err != nil {
		return fixer, ""
	}
	if _, err := gitx.Run(a.Root, "update-ref", ciFixRef(task), head); err != nil {
		return fixer, ""
	}
	a.Store.Event(task, eventCIRepairFixer, fixer+" "+head)
	return fixer, head
}

// ciRepairPrompt tells the repair what is red and that its fix joins the
// red layer.
func (a *App) ciRepairPrompt(owner store.Task, l CIRedLayer, fails []CIRedFailure, fixer, fixCommit string, attempt int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CI is red on the PR for %s %q (%s), at its head %s: %s. It has landed and its agent is gone, so you fix it.\n\n",
		owner.ID, owner.Title, l.PR, short(l.Head), strings.Join(l.Checks, ", "))
	for _, f := range fails {
		fmt.Fprintf(&b, "%s failed", f.Check)
		if f.Step != "" {
			fmt.Fprintf(&b, " in step %q", f.Step)
		}
		if f.RunURL != "" {
			fmt.Fprintf(&b, " (%s)", f.RunURL)
		}
		b.WriteString(".\n")
		if f.LogTail != "" {
			b.WriteString("Log tail:\n" + f.LogTail + "\n")
		}
		b.WriteString("\n")
	}
	if fixCommit != "" {
		fmt.Fprintf(&b, "This looks like a lint or format failure, and saddle already ran the repo's fixer `%s` on %s's layer. "+
			"Start with `git cherry-pick %s` (commit %s), then check whether anything is still red.\n\n", fixer, owner.ID, ciFixRef(owner.ID), short(fixCommit))
	} else if fixer != "" {
		fmt.Fprintf(&b, "This looks like a lint or format failure; the repo's fixer is `%s`. Run it first.\n\n", fixer)
	}
	fmt.Fprintf(&b, "Your branch is cut from %s, so it has %s's work (branch %s). Reproduce the failure, fix it with the smallest change, "+
		"run the tests, commit, and call the saddle done tool. When your branch lands, saddle folds your commits into %s's layer: "+
		"they go out on %s's PR, not as a new PR, and the layers stacked above it wait until its CI is green. "+
		"Fix only what makes %s's CI red. Don't touch %s yourself and don't open a PR. This is repair attempt %d of %d.",
		a.Cfg.Integration, owner.ID, owner.Branch, owner.ID, owner.ID, owner.ID, owner.Branch, attempt, a.Cfg.CI.RepairAttempts)
	return b.String()
}

// tokenRe finds names a failure log may mention: paths, targets, symbols.
var tokenRe = regexp.MustCompile(`[A-Za-z0-9_][A-Za-z0-9_./-]*[./_-][A-Za-z0-9_./-]*[A-Za-z0-9_]`)

// failureTokens are the names in the last lines of the failures' logs.
func failureTokens(fails []CIRedFailure) []string {
	var out []string
	for _, f := range fails {
		lines := strings.Split(f.LogTail, "\n")
		if len(lines) > 15 {
			lines = lines[len(lines)-15:]
		}
		for _, tok := range tokenRe.FindAllString(strings.Join(lines, "\n"), -1) {
			if len(tok) >= 5 && !strings.HasPrefix(tok, "http") && !slices.Contains(out, tok) {
				out = append(out, tok)
			}
		}
	}
	return out
}

// explainedBy names the first token a sibling's added lines introduce and
// the red layer's base lacks: the failure needs the sibling's work.
func explainedBy(tokens []string, added string, baseHas func(string) bool) (string, bool) {
	for _, tok := range tokens {
		if strings.Contains(added, tok) && !baseHas(tok) {
			return tok, true
		}
	}
	return "", false
}

// siblingExplains is the default #193 test: some stacked task not below
// task in its PR stack adds a name its failing log mentions, and the base
// task's PR is built on lacks that name.
func (a *App) siblingExplains(task string, fails []CIRedFailure) (string, bool) {
	toks := failureTokens(fails)
	if len(toks) == 0 {
		return "", false
	}
	stack, err := a.landedStack()
	if err != nil {
		return "", false
	}
	at := slices.IndexFunc(stack, func(l landedTask) bool { return l.ID == task })
	if at < 0 {
		return "", false
	}
	layout, _, err := a.stackLayout(stack)
	if err != nil {
		return "", false
	}
	base := "refs/remotes/origin/" + a.Cfg.Base
	if b := layout[at].Below; b >= 0 {
		base = layout[b].Head
	}
	baseHas := func(tok string) bool {
		_, err := gitx.Run(a.Root, "grep", "-q", "-F", "-e", tok, base)
		return err == nil
	}
	for j, l := range stack {
		if j == at || above(layout, at, j) || l.From == "" || l.Lost != "" {
			continue // the layers below it are in its base already
		}
		diff, err := gitx.Run(a.Root, "diff", "--unified=0", l.From, l.To)
		if err != nil {
			continue
		}
		var added strings.Builder
		for _, line := range strings.Split(diff, "\n") {
			if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
				added.WriteString(line[1:] + "\n")
			}
		}
		if tok, ok := explainedBy(toks, added.String(), baseHas); ok {
			a.Store.Event(task, "ci_red_sibling", l.ID+": "+tok)
			return l.ID, true
		}
	}
	return "", false
}

// FoldCIRepairs folds every landed CI repair into the red layer it repaired:
// its train entry moves right after that layer's, restack replays the stack
// in that order, and the layer's range grows to cover the repair's commits.
// The repair leaves the stack as folded, and a second restack pushes the red
// layer's branch, so the fix lands on its PR. It returns the repairs folded.
func (a *App) FoldCIRepairs() ([]string, error) {
	s, err := a.CIRed()
	if err != nil {
		return nil, err
	}
	var done []string
	var errs []error
	for i, rec := range s.Repairs {
		if rec.State != ciRepairSpawned {
			continue
		}
		entry, ok := a.trainEntry(rec.Repair)
		if !ok || entry.State != store.TrainOK {
			continue // not landed yet
		}
		if err := a.foldCIRepair(rec); err != nil {
			s.Repairs[i].State, s.Repairs[i].Note = ciRepairFailed, err.Error()
			a.Store.Event(rec.Task, "ci_repair_fold_failed", rec.Repair+": "+err.Error())
			errs = append(errs, a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
				"%s landed but couldn't be folded into %s's layer: %v\nIt stays in the stack as its own layer; decide whether to `saddle unstack %s` or keep it.",
				rec.Repair, rec.Task, err, rec.Repair)))
			continue
		}
		s.Repairs[i].State = ciRepairFolded
		done = append(done, rec.Repair)
		a.Store.Event(rec.Task, eventCIRepairFolded, rec.Repair)
		errs = append(errs, a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf(
			"Folded %s's CI repair %s into %s's layer and pushed it to %s's PR; the hold above it lifts once its checks pass.",
			rec.Task, rec.Repair, rec.Task, rec.Task)))
	}
	for i := range s.Red {
		if r, ok := a.liveCIRepair(s, s.Red[i].Task); ok {
			s.Red[i].Repair = r
		} else {
			s.Red[i].Repair = ""
		}
	}
	if len(done) > 0 || len(errs) > 0 {
		errs = append(errs, a.SetCIRed(s))
	}
	return done, errors.Join(errs...)
}

// foldCIRepair folds one landed repair into its red layer, under the train
// lock: it replays the repair's commits right onto the red layer's, then
// the layers after it, moves the refs as restack does, grows the red
// layer's range over the repair's commits and pushes what moved.
func (a *App) foldCIRepair(rec CIRepairRecord) error {
	unlock, err := a.lockTrain()
	if err != nil {
		return err
	}
	defer unlock()
	stack, err := a.landedStack()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(stack, func(l landedTask) bool { return l.ID == rec.Task })
	j := slices.IndexFunc(stack, func(l landedTask) bool { return l.ID == rec.Repair })
	switch {
	case i < 0 || j < 0:
		return fmt.Errorf("%s or %s isn't in the PR stack", rec.Task, rec.Repair)
	case j < i:
		return fmt.Errorf("%s landed before %s", rec.Repair, rec.Task)
	}
	for _, l := range stack[i:] {
		if l.Lost != "" || l.From == "" {
			return fmt.Errorf("can't tell %s's landed work apart", l.ID)
		}
	}
	red, fix := stack[i], stack[j]
	sub := []landedTask{fix}
	for k, l := range stack[i+1:] {
		if i+1+k != j {
			sub = append(sub, l)
		}
	}
	integ, err := gitx.RevParse(a.Root, a.Cfg.Integration)
	if err != nil {
		return err
	}
	plan, _, err := a.replay(sub, red.To, map[string]bool{})
	if err != nil {
		return err
	}
	if plan[0].gone() {
		return fmt.Errorf("%s's commits change nothing on %s's layer", fix.ID, red.ID)
	}
	var res RestackResult
	if err := a.moveStack(plan, integ, red.To, &res); err != nil {
		return err
	}
	grown := plan[0].NewTo
	if _, err := trainGit(a.Root, "update-ref", "refs/heads/"+red.Branch, grown, red.To); err != nil {
		return err
	}
	if err := errors.Join(
		a.Store.SetTrain(red.ID, store.TrainOK, red.From+".."+grown, false),
		a.Store.SetTrain(fix.ID, TrainFolded, plan[0].NewFrom+".."+grown, false),
		a.trainAfter(fix.ID, red.ID),
	); err != nil {
		return err
	}
	a.Store.Event(fix.ID, "unstacked", TrainFolded+": its commits joined "+red.ID+"'s layer")
	a.Store.Event(red.ID, "restack", fmt.Sprintf("refs/heads/%s %s → %s (folded %s)", red.Branch, short(red.To), short(grown), fix.ID))
	// Push the grown layer and the layers that moved above it.
	var full []restacked
	for _, l := range stack[:i] {
		full = append(full, restacked{landedTask: l, NewFrom: l.From, NewTo: l.To})
	}
	full = append(full, restacked{landedTask: red, NewFrom: red.From, NewTo: grown})
	full = append(full, plan[1:]...)
	return a.republish(full, &res)
}

// trainAfter moves task's train entry to right after after's.
func (a *App) trainAfter(task, after string) error {
	es, err := a.Store.Train()
	if err != nil {
		return err
	}
	var order []string
	for _, e := range es {
		if e.Task == task {
			continue
		}
		order = append(order, e.Task)
		if e.Task == after {
			order = append(order, task)
		}
	}
	if !slices.Contains(order, task) {
		return fmt.Errorf("%s isn't in the train", after)
	}
	return a.Store.SetTrainOrder(order)
}
