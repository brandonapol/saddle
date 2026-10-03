package app

import (
	"errors"
	"fmt"
	"slices"

	"github.com/brandonapol/saddle/internal/automerge"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// The auto-merge watcher (#152) lives in internal/automerge; this wires it to
// the stack: which tasks have PRs, the at-risk flag, restack, events and
// notices. Its toggle and holds are a file under .saddle/, not the store.

func (a *App) automergePath() string { return a.stateDir("automerge.json") }

// NewAutomerge returns the auto-merge watcher for the stack, talking to
// GitHub through gh; nil gh means the gh CLI in the repo.
func (a *App) NewAutomerge(gh automerge.GitHub) *automerge.Watcher {
	if gh == nil {
		gh = &automerge.GH{Run: automerge.ExecRunner(a.Root)}
	}
	return &automerge.Watcher{
		Path:     a.automergePath(),
		Default:  a.Cfg.Train.AutoMerge,
		Base:     a.Cfg.Base,
		GH:       gh,
		Entries:  a.automergeEntries,
		AtRisk:   a.flagCovers,
		Behind:   a.behindBase,
		Restack:  func() error { _, err := a.Restack(); return err },
		Lock:     a.TryLockTrain,
		Collapse: func(s string) (string, error) { return a.AutoCollapse(s, gh) },
		Event:    a.Store.Event,
		Notify: func(action bool, text string) {
			kind := store.NoticeInfo
			if action {
				kind = store.NoticeAction
			}
			_ = a.Notify(OrchestratorID, kind, text)
		},
	}
}

// AutomergeState is the auto-merge state as last saved: on or off and why,
// holds, a stop after a failure, and the stacks the last check saw. It asks
// GitHub nothing, so the TUI can read it every frame.
func (a *App) AutomergeState() (automerge.Status, error) {
	return a.NewAutomerge(noGitHub{}).Status()
}

// noGitHub stands in where nothing may reach GitHub.
type noGitHub struct{}

var errNoGitHub = errors.New("GitHub isn't asked here")

func (noGitHub) PR(string) (automerge.PR, error)    { return automerge.PR{}, errNoGitHub }
func (noGitHub) MergeMethod() (string, error)       { return "", errNoGitHub }
func (noGitHub) Merge(string, string, string) error { return errNoGitHub }

// AutomergeHold holds the stack of ref (stack name, task id, PR URL or
// number): it is never auto-merged until released, but stays tracked and
// restacked.
func (a *App) AutomergeHold(ref string) (store.Task, error) {
	t, err := a.taskByRef(ref)
	if err != nil {
		return t, err
	}
	return t, a.NewAutomerge(nil).Hold(t.ID)
}

// AutomergeRelease lifts every hold on ref's stack.
func (a *App) AutomergeRelease(ref string) (store.Task, error) {
	t, err := a.taskByRef(ref)
	if err != nil {
		return t, err
	}
	return t, a.NewAutomerge(nil).Release(t.ID)
}

// automergeEntries lists the stacked tasks with PRs in train order, after
// fetching base so Behind counts against what origin has.
func (a *App) automergeEntries() ([]automerge.Entry, error) {
	_, _ = gitx.Run(a.Root, "fetch", "--quiet", "origin", "+refs/heads/"+a.Cfg.Base+":refs/remotes/origin/"+a.Cfg.Base)
	stack, err := a.landedStack()
	if err != nil {
		return nil, err
	}
	var out []automerge.Entry
	for _, l := range stack {
		if l.PR != "" {
			out = append(out, automerge.Entry{Task: l.ID, Branch: l.Branch, PR: l.PR})
		}
	}
	return out, nil
}

// flagCovers says why the stack-at-risk flag covers task: it is the flagged
// layer or above it in train order. An acked flag still covers it; auto-merge
// leaves that to a human.
func (a *App) flagCovers(task string) string {
	f, ok, err := a.Flag()
	if err != nil {
		return "can't read the stack flag: " + err.Error()
	}
	if !ok {
		return ""
	}
	stack, err := a.landedStack()
	if err != nil {
		return "can't read the stack: " + err.Error()
	}
	from := slices.IndexFunc(stack, func(l landedTask) bool { return l.ID == f.Task })
	at := slices.IndexFunc(stack, func(l landedTask) bool { return l.ID == task })
	if from >= 0 && at >= 0 && at < from {
		return ""
	}
	why := fmt.Sprintf("from %s up: %s", f.Task, f.Cause)
	if f.Acked {
		why += " (acknowledged)"
	}
	return why
}

// behindBase counts the commits origin/<base> has that head lacks; 0 when it
// can't tell.
func (a *App) behindBase(head string) int {
	n, err := gitx.CommitsBetween(a.Root, head, "refs/remotes/origin/"+a.Cfg.Base)
	if err != nil {
		return 0
	}
	return n
}

// StackRebase is what `saddle stack rebase` did.
type StackRebase struct {
	Stack   string        `json:"stack"` // the stack's bottom task
	Tasks   []string      `json:"tasks"` // its tasks, bottom first
	Moves   []RestackMove `json:"moves,omitempty"`
	Restack RestackResult `json:"restack"`
}

// RebaseStack rebases ref's stack (stack name, task id, PR URL or number)
// onto origin/<base>, held or not. The integration branch is one linear
// history, so this is restack: every stacked task is replayed in train order
// and nothing moves unless all of them apply. A conflict stops it and goes
// back to the task that owns the commit. The result reports the moves of
// ref's stack.
func (a *App) RebaseStack(ref string) (StackRebase, error) {
	var out StackRebase
	t, err := a.taskByRef(ref)
	if err != nil {
		return out, err
	}
	stack, err := a.landedStack()
	if err != nil {
		return out, err
	}
	if !slices.ContainsFunc(stack, func(l landedTask) bool { return l.ID == t.ID }) {
		return out, fmt.Errorf("%s isn't in the PR stack, so there is nothing to rebase", t.ID)
	}
	res, err := a.Restack()
	out.Restack = res
	if err != nil {
		return out, err
	}
	out.Stack, out.Tasks = t.ID, []string{t.ID}
	if members, err := a.stackOf(t.ID); err == nil && len(members) > 0 {
		out.Stack, out.Tasks = members[0], members
	}
	for _, m := range res.Moves {
		if slices.Contains(out.Tasks, m.Task) {
			out.Moves = append(out.Moves, m)
		}
	}
	return out, nil
}

// stackOf lists the tasks in task's published PR stack, bottom first.
func (a *App) stackOf(task string) ([]string, error) {
	unlock, err := a.lockTrain()
	if err != nil {
		return nil, err
	}
	defer unlock()
	stack, err := a.landedStack()
	if err != nil {
		return nil, err
	}
	layout, err := a.prLayout(stack)
	if err != nil {
		return nil, err
	}
	at := slices.IndexFunc(stack, func(l landedTask) bool { return l.ID == task })
	if at < 0 {
		return nil, nil
	}
	var out []string
	for i, l := range stack {
		if layout[i].Group == layout[at].Group {
			out = append(out, l.ID)
		}
	}
	return out, nil
}
