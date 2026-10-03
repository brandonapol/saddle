package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// A conflict needs an owner (#172). Restack and land hand a conflict back to
// the task that made the commit, but a landed task's window is usually gone
// (close_on_land), so nobody would resolve it and the stack stayed flagged.
// When the owner has no live window of its own, saddle spawns a repair task
// instead: once per conflict, cut from integration, seeded with the
// original's branch, commits, the conflicting files, the base and what the
// original was asked to do. The repair resolves the conflict on its own
// branch and lands normally; then the original is superseded.

// TrainRepairing is the train state of a landed task whose commits conflict
// with base while a repair task re-lands its work. It is out of the stack:
// restack drops its commits so the rest of the stack moves on, and saddle
// leaves its PR alone until the repair lands and supersedes it.
const TrainRepairing = "repairing"

// Repair events, recorded on the original task.
const (
	eventRepairNeeded  = "repair_needed"  // data: the repairSeed as JSON
	eventRepairSpawned = "repair_spawned" // data: the repair task's id
	eventRepairFailed  = "repair_failed"  // data: why the repair couldn't be spawned
)

// repairSeed is what a repair task starts from.
type repairSeed struct {
	Source string   `json:"source"`           // restack or land
	Commit string   `json:"commit,omitempty"` // the commit that stopped
	Files  []string `json:"files,omitempty"`  // the files it conflicts in
	Onto   string   `json:"onto"`             // the branch it conflicted with
	Base   string   `json:"base,omitempty"`   // that branch's commit then
	From   string   `json:"from,omitempty"`   // the original's own commits: From..To
	To     string   `json:"to,omitempty"`
}

// RepairResult is what Repair did.
type RepairResult struct {
	Task    string `json:"task"`   // the original
	Repair  string `json:"repair"` // the repair task, "" if it couldn't be spawned
	Created bool   `json:"created"`
	Note    string `json:"note,omitempty"`
}

func repairTitlePrefix(orig string) string { return "Repair " + orig + " " }

// orphaned reports whether task has no live agent window of its own to take
// a conflict.
func (a *App) orphaned(task string) bool {
	t, err := a.Store.Task(task)
	return err == nil && t.Role == store.RoleWorker && !a.ownWindow(t)
}

// repairOf finds the live or landed repair task spawned for orig.
func (a *App) repairOf(orig string) (store.Task, bool) {
	es, err := a.Store.Events(-1)
	if err != nil {
		return store.Task{}, false
	}
	for i := len(es) - 1; i >= 0; i-- {
		e := es[i]
		if e.Task != orig || e.Kind != eventRepairSpawned {
			continue
		}
		if t, err := a.Store.Task(e.Data); err == nil && t.Status != store.Killed {
			return t, true
		}
	}
	return store.Task{}, false
}

// repairedBy finds the original task the repair task r was spawned for.
func (a *App) repairedBy(r string) (string, bool) {
	es, err := a.Store.Events(-1)
	if err != nil {
		return "", false
	}
	for i := len(es) - 1; i >= 0; i-- {
		if e := es[i]; e.Kind == eventRepairSpawned && e.Data == r {
			return e.Task, true
		}
	}
	return "", false
}

// lastEvent is the data of the last event of kind on task.
func (a *App) lastEvent(task, kind string) (string, bool) {
	es, err := a.Store.Events(-1)
	if err != nil {
		return "", false
	}
	for i := len(es) - 1; i >= 0; i-- {
		if e := es[i]; e.Task == task && e.Kind == kind {
			return e.Data, true
		}
	}
	return "", false
}

// markRepairing takes a landed task out of the stack for a repair and
// records what the repair starts from.
func (a *App) markRepairing(l landedTask, seed repairSeed) error {
	seed.From, seed.To = l.From, l.To
	b, _ := json.Marshal(seed)
	a.Store.Event(l.ID, eventRepairNeeded, string(b))
	if err := a.Store.SetTrain(l.ID, TrainRepairing, l.rangeNote(), false); err != nil {
		return err
	}
	a.Store.Event(l.ID, "unstacked", TrainRepairing+": "+short(seed.Commit)+" conflicts with "+seed.Onto+" and its agent is gone")
	return nil
}

// seedOf is the last repair seed recorded for task.
func (a *App) seedOf(task string) (repairSeed, bool) {
	var s repairSeed
	data, ok := a.lastEvent(task, eventRepairNeeded)
	if !ok || json.Unmarshal([]byte(data), &s) != nil {
		return s, false
	}
	return s, true
}

// spawnRepairs makes sure every repairing task has a repair task, and
// returns the ids it spawned.
func (a *App) spawnRepairs() []string {
	all, err := a.landedAll()
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range all {
		if l.State != TrainRepairing {
			continue
		}
		seed, _ := a.seedOf(l.ID)
		if r, created, err := a.ensureRepair(l.Task, seed, false); err == nil && created {
			out = append(out, r.ID)
		}
	}
	return out
}

// ensureRepair spawns orig's repair task unless one is already live or
// landed. The orchestrator hears once: when the repair is spawned, or when it
// can't be, once per reason.
func (a *App) ensureRepair(orig store.Task, seed repairSeed, force bool) (store.Task, bool, error) {
	if r, ok := a.repairOf(orig.ID); ok {
		return r, false, nil
	}
	want := slices.Clone(seed.Files)
	cl, err := a.Store.Claims()
	if err != nil {
		return store.Task{}, false, err
	}
	own := cl[orig.ID]
	if len(own) == 0 {
		if data, ok := a.lastEvent(orig.ID, landedClaimsEvent); ok && data != "" {
			own = strings.Split(data, "\n")
		}
	}
	for _, c := range own {
		if !slices.Contains(want, c) {
			want = append(want, c)
		}
	}
	// The original has no agent: its claims pass to the repair.
	if len(cl[orig.ID]) > 0 {
		if err := a.Store.Release(orig.ID); err != nil {
			return store.Task{}, false, err
		}
	}
	req := SpawnReq{
		Title:   repairTitlePrefix(orig.ID) + orig.Title,
		Prompt:  a.repairPrompt(orig, seed),
		Parent:  OrchestratorID,
		Claims:  want,
		Issue:   orig.Issue,
		Force:   force,
		Confirm: true,
	}
	r, err := a.Spawn(req)
	if err != nil && !force && len(seed.Files) > 0 && len(want) > len(seed.Files) {
		// The original's claims may overlap live work by now; the conflicting
		// files are what the repair must own.
		req.Claims = slices.Clone(seed.Files)
		r, err = a.Spawn(req)
	}
	if err != nil {
		why := err.Error()
		if last, ok := a.lastEvent(orig.ID, eventRepairFailed); !ok || last != why {
			a.Store.Event(orig.ID, eventRepairFailed, why)
			_ = a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
				"%s's %s conflicts with %s in %s and its agent is gone, but saddle couldn't spawn a repair task: %s\n"+
					"Run `saddle repair %s` to try again (--force ignores the concurrency limit and claim overlaps).",
				orig.ID, what(seed), seed.Onto, strings.Join(seed.Files, ", "), why, orig.ID))
		}
		return store.Task{}, false, err
	}
	a.Store.Event(orig.ID, eventRepairSpawned, r.ID)
	_ = a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
		"%s's %s conflicts with %s in %s and its agent is gone, so saddle spawned %s %q to resolve it on a fresh branch. "+
			"When %s lands, %s is superseded and its PR closed. Nothing to do unless %s gets stuck.",
		orig.ID, what(seed), seed.Onto, strings.Join(seed.Files, ", "), r.ID, r.Title, r.ID, orig.ID, r.ID))
	return r, true, nil
}

// what names the work a seed is about, for notices.
func what(s repairSeed) string {
	if s.Source == "land" {
		return "branch"
	}
	if s.Commit != "" {
		return "landed commit " + short(s.Commit)
	}
	return "landed work"
}

// repairPrompt tells the repair task what it is re-landing and how.
func (a *App) repairPrompt(orig store.Task, s repairSeed) string {
	var b strings.Builder
	files := strings.Join(s.Files, ", ")
	if files == "" {
		files = "(unknown; cherry-pick to find them)"
	}
	fmt.Fprintf(&b, "You are repairing %s %q. Its work conflicts with %s", orig.ID, orig.Title, s.Onto)
	if s.Base != "" {
		fmt.Fprintf(&b, " at %s", short(s.Base))
	}
	fmt.Fprintf(&b, " in %s, and its agent is gone, so nobody else will resolve it.\n\n", files)
	if strings.TrimSpace(orig.Prompt) != "" {
		fmt.Fprintf(&b, "What %s was asked to do:\n%s\n\n", orig.ID, strings.TrimSpace(orig.Prompt))
	}
	if strings.TrimSpace(orig.Summary) != "" {
		fmt.Fprintf(&b, "What %s reported when it finished:\n%s\n\n", orig.ID, strings.TrimSpace(orig.Summary))
	}
	rng := orig.Branch
	if s.From != "" && s.To != "" {
		rng = s.From + ".." + s.To
	}
	fmt.Fprintf(&b, "Its work: branch %s, commits %s", orig.Branch, rng)
	if s.Commit != "" {
		fmt.Fprintf(&b, "; commit %s is the one that stops", short(s.Commit))
	}
	b.WriteString(".\n")
	if s.Source == "land" {
		fmt.Fprintf(&b, "Your branch is cut from %s, which doesn't have %s's work.\n", a.Cfg.Integration, orig.ID)
	} else {
		fmt.Fprintf(&b, "Your branch is cut from %s, rebuilt on %s without %s's commits.\n", a.Cfg.Integration, s.Onto, orig.ID)
	}
	fmt.Fprintf(&b, "1. `git cherry-pick %s` (it stops at the conflicts).\n", rng)
	fmt.Fprintf(&b, "2. Resolve %s keeping both sides' intent: what %s now has and what %s meant to do. `git add` them and `git cherry-pick --continue`.\n", files, s.Onto, orig.ID)
	b.WriteString("3. Run the tests, commit, and call the saddle done tool. Your branch lands like any other.\n")
	fmt.Fprintf(&b, "When it lands, %s is superseded and its PR closed in favor of yours. Don't touch %s.", orig.ID, orig.Branch)
	return b.String()
}

// repairLanded supersedes the original task once its repair r has landed. It
// returns a sentence for the landing notice, or "".
func (a *App) repairLanded(r string) string {
	orig, ok := a.repairedBy(r)
	if !ok {
		return ""
	}
	all, err := a.landedAll()
	if err != nil {
		return ""
	}
	why := "its repair " + r + " landed"
	for _, l := range all {
		if l.ID != orig {
			continue
		}
		if l.State != TrainRepairing && l.State != store.TrainOK {
			return ""
		}
		_ = a.Store.SetTrain(l.ID, TrainSuperseded, l.rangeNote(), false)
		a.Store.Event(l.ID, "unstacked", TrainSuperseded+": "+why)
		a.closeSuperseded(l.Task, r)
		return fmt.Sprintf(" It repairs %s, which is now superseded%s.", orig, prClosed(l.PR))
	}
	// Never landed: a branch whose land conflicted.
	t, err := a.Store.Task(orig)
	if err != nil || !t.Active() {
		return ""
	}
	_ = errors.Join(
		a.Store.SetTrain(orig, TrainSuperseded, why, false),
		a.Store.Release(orig),
		a.Store.SetStatus(orig, store.Killed),
	)
	a.Store.Event(orig, "superseded", why)
	return fmt.Sprintf(" It repairs %s, which is now superseded.", orig)
}

func prClosed(pr string) string {
	if pr == "" {
		return ""
	}
	return " and its PR " + pr + " closed"
}

// closeSuperseded closes orig's PR with a comment naming the repair.
func (a *App) closeSuperseded(orig store.Task, r string) {
	if orig.PR == "" {
		return
	}
	body := fmt.Sprintf("Superseded by %s, saddle's repair of %s: this PR's commits conflicted with %s after it landed and its agent was gone. "+
		"%s re-lands the same work resolved on %s and gets its own PR.", r, orig.ID, a.Cfg.Base, r, a.Cfg.Base)
	if _, err := gh(a.Root, "pr", "comment", orig.PR, "--body", body); err != nil {
		a.Store.Event(orig.ID, "pr_close_error", err.Error())
	}
	if _, err := gh(a.Root, "pr", "close", orig.PR); err != nil {
		a.Store.Event(orig.ID, "pr_close_error", err.Error())
	}
}

// Repair spawns a repair task for ref (task id, PR URL or number) by hand,
// or returns the one it already has. A stacked task leaves the stack as
// repairing and restack runs, so the repair is cut from integration without
// its commits; a task whose land conflicted gets its repair at once. force
// spawns past the concurrency limit and claim overlaps.
func (a *App) Repair(ref string, force bool) (RepairResult, error) {
	t, err := a.taskByRef(ref)
	if err != nil {
		return RepairResult{}, err
	}
	res := RepairResult{Task: t.ID}
	if r, ok := a.repairOf(t.ID); ok {
		res.Repair, res.Note = r.ID, r.ID+" is already repairing "+t.ID
		return res, nil
	}
	entry, ok := a.trainEntry(t.ID)
	if !ok {
		return res, fmt.Errorf("%s isn't in the merge train; there is nothing to repair", t.ID)
	}
	var seed repairSeed
	switch entry.State {
	case store.TrainOK:
		if err := a.markForRepair(t); err != nil {
			return res, err
		}
		if _, err := a.Restack(); err != nil {
			res.Note = "restack failed, so the repair waits for the next one: " + err.Error()
			return res, nil
		}
		if r, ok := a.repairOf(t.ID); ok {
			res.Repair, res.Created = r.ID, true
			return res, nil
		}
		seed, _ = a.seedOf(t.ID)
	case TrainRepairing:
		seed, _ = a.seedOf(t.ID)
	case store.TrainError, TrainEscalated, store.TestFailed:
		seed = a.landSeed(t, entry.Note)
	default:
		return res, fmt.Errorf("%s is %s in the train; only a landed task or one whose land failed can be repaired", t.ID, entry.State)
	}
	r, created, err := a.ensureRepair(t, seed, force)
	if err != nil {
		return res, err
	}
	res.Repair, res.Created = r.ID, created
	return res, nil
}

// trainEntry is task's train entry.
func (a *App) trainEntry(task string) (store.TrainEntry, bool) {
	es, err := a.Store.Train()
	if err != nil {
		return store.TrainEntry{}, false
	}
	for _, e := range es {
		if e.Task == task {
			return e, true
		}
	}
	return store.TrainEntry{}, false
}

// markForRepair takes stacked task t out of the stack for a hand-triggered
// repair, seeded from its last restack conflict or, failing that, every file
// it changed.
func (a *App) markForRepair(t store.Task) error {
	unlock, err := a.lockTrain()
	if err != nil {
		return err
	}
	defer unlock()
	all, err := a.landedAll()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(all, func(l landedTask) bool { return l.ID == t.ID })
	if i < 0 || !all[i].stacked() {
		return fmt.Errorf("%s isn't in the PR stack", t.ID)
	}
	l := all[i]
	if l.Lost != "" {
		return fmt.Errorf("can't repair %s: %s", l.ID, l.Lost)
	}
	seed := repairSeed{Source: "restack", Onto: "origin/" + a.Cfg.Base}
	seed.Base, _ = gitx.RevParse(a.Root, "refs/remotes/origin/"+a.Cfg.Base)
	if data, ok := a.lastEvent(t.ID, "restack_conflict"); ok {
		c, files, _ := strings.Cut(data, " ")
		seed.Commit, seed.Files = c, strings.Split(files, ", ")
	} else {
		seed.Files, _ = gitx.ChangedFiles(a.Root, l.From, l.To)
	}
	return a.markRepairing(l, seed)
}

// landSeed seeds the repair of a branch whose land failed: note is the
// train's note, the conflicting files.
func (a *App) landSeed(t store.Task, note string) repairSeed {
	s := repairSeed{Source: "land", Onto: a.Cfg.Integration}
	s.Base, _ = gitx.RevParse(a.Root, a.Cfg.Integration)
	for _, f := range strings.Split(note, ", ") {
		if f = strings.TrimSpace(f); f != "" && !strings.Contains(f, " ") {
			s.Files = append(s.Files, f)
		}
	}
	if from, _, err := a.replayFrom(t, a.Cfg.Integration); err == nil {
		s.From = from
		s.To, _ = gitx.RevParse(a.Root, t.Branch)
	}
	return s
}
