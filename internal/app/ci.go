package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/brandonapol/saddle/internal/ciwatch"
	"github.com/brandonapol/saddle/internal/store"
)

// EventCIError is the events-table kind for a failed gh call or state write
// in the CI watcher.
const EventCIError = "ci_error"

// CIWatcher polls the checks on saddle PRs and routes changes: a failure
// goes to the owning task if it is still active, else to a fix task it
// spawns, and always to the orchestrator; a recovery goes out as info. What
// it has seen is saved under .saddle so a restart does not re-report it.
type CIWatcher struct {
	a *App
	w *ciwatch.Watcher
}

// NewCIWatcher returns a watcher that runs gh through gh (ciwatch.ExecRunner
// for the real one) and picks up any state saved by an earlier one.
func (a *App) NewCIWatcher(gh ciwatch.Runner) (*CIWatcher, error) {
	w, err := ciwatch.New(ciwatch.Config{
		GH:       gh,
		Targets:  a.CITargets,
		Interval: a.Cfg.CI.Interval,
		OnError: func(t ciwatch.Target, err error) {
			a.Store.Event(t.Task, EventCIError, err.Error())
		},
	})
	if err != nil {
		return nil, err
	}
	c := &CIWatcher{a: a, w: w}
	if seen, err := c.load(); err != nil {
		a.Store.Event("", EventCIError, "read CI state: "+err.Error())
	} else if seen != nil {
		w.Restore(seen)
	}
	return c, nil
}

// CITargets lists the PRs to watch: every live task that has one.
func (a *App) CITargets(context.Context) ([]ciwatch.Target, error) {
	ts, err := a.Store.Tasks()
	if err != nil {
		return nil, err
	}
	var out []ciwatch.Target
	for _, t := range ts {
		if t.PR != "" && t.Status != store.Killed {
			out = append(out, ciwatch.Target{Task: t.ID, Branch: t.Branch, PR: t.PR})
		}
	}
	return out, nil
}

// Run polls now and then every ci.interval until ctx is done.
func (c *CIWatcher) Run(ctx context.Context) {
	c.w.Run(ctx, c.handle)
}

// Poll checks every PR once and routes what changed.
func (c *CIWatcher) Poll(ctx context.Context) {
	for _, e := range c.w.Poll(ctx) {
		c.handle(e)
	}
	c.save()
}

func (c *CIWatcher) handle(e ciwatch.Event) {
	var err error
	switch e := e.(type) {
	case ciwatch.Failed:
		err = c.a.ciFailed(e)
	case ciwatch.Recovered:
		err = c.a.ciRecovered(e)
	}
	if err != nil {
		c.a.Store.Event(e.From().Task, EventCIError, err.Error())
	}
	// Run has no hook after a poll, so state is saved after every event.
	c.save()
}

func (a *App) ciFailed(f ciwatch.Failed) error {
	owner, err := a.Store.Task(f.Task)
	if err != nil {
		return a.Notify(OrchestratorID, store.NoticeAction, f.Report())
	}
	a.Store.Event(owner.ID, "ci_failed", f.Check.Label()+" "+f.RunURL)
	about := fmt.Sprintf("%s %q: %s", owner.ID, owner.Title, f.Report())
	if owner.Active() {
		if err := a.Notify(owner.ID, store.NoticeAction, f.Message()); err != nil {
			return err
		}
		return a.Notify(OrchestratorID, store.NoticeAction, about+"\n"+owner.ID+" was told to fix it.")
	}
	if fix, ok := a.ciFixTask(owner); ok {
		if err := a.Notify(fix.ID, store.NoticeAction, f.Report()+"\nFix it on your branch, commit, and call the saddle done tool again."); err != nil {
			return err
		}
		return a.Notify(OrchestratorID, store.NoticeAction, about+"\n"+fix.ID+" is already fixing it.")
	}
	fix, err := a.Spawn(SpawnReq{
		Title:  ciFixTitle(owner),
		Prompt: ciFixPrompt(owner, f),
		Parent: OrchestratorID,
	})
	if err != nil {
		return a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
			"%s\n%s has landed, and a fix task could not be spawned: %v\nSpawn one titled %q when you can.",
			about, owner.ID, err, ciFixTitle(owner)))
	}
	return a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf("%s\n%s has landed, so I spawned %s to fix it.", about, owner.ID, fix.ID))
}

func (a *App) ciRecovered(r ciwatch.Recovered) error {
	msg := r.Message()
	var errs []error
	if owner, err := a.Store.Task(r.Task); err == nil {
		a.Store.Event(owner.ID, "ci_recovered", r.Check.Label())
		if owner.Active() {
			errs = append(errs, a.Notify(owner.ID, store.NoticeInfo, msg))
		} else if fix, ok := a.ciFixTask(owner); ok {
			errs = append(errs, a.Notify(fix.ID, store.NoticeInfo, msg))
		}
	}
	errs = append(errs, a.Notify(OrchestratorID, store.NoticeInfo, msg))
	return errors.Join(errs...)
}

func ciFixTitle(owner store.Task) string {
	return "Fix CI for " + owner.ID + " " + owner.Title
}

// ciFixTask finds the live fix task spawned for owner's CI, if any.
func (a *App) ciFixTask(owner store.Task) (store.Task, bool) {
	ts, err := a.Store.Tasks()
	if err != nil {
		return store.Task{}, false
	}
	title := ciFixTitle(owner)
	for _, t := range ts {
		if t.Active() && t.Title == title {
			return t, true
		}
	}
	return store.Task{}, false
}

func ciFixPrompt(owner store.Task, f ciwatch.Failed) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CI is failing on the PR for %s %q, which has already landed on the integration branch.\n\n", owner.ID, owner.Title)
	b.WriteString(f.Report())
	fmt.Fprintf(&b, "\n\nYour branch is cut from the integration branch, so it already has %s's work. "+
		"Reproduce the failure, write a failing test if one is missing, fix it on your branch, "+
		"run the tests, commit, and call the saddle done tool. Do not touch %s.", owner.ID, owner.Branch)
	return b.String()
}

func (c *CIWatcher) path() string { return c.a.stateDir("ci-seen.json") }

func (c *CIWatcher) load() (map[string]map[string]ciwatch.Seen, error) {
	b, err := os.ReadFile(c.path())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var seen map[string]map[string]ciwatch.Seen
	return seen, json.Unmarshal(b, &seen)
}

func (c *CIWatcher) save() {
	b, err := json.Marshal(c.w.Snapshot())
	if err == nil {
		tmp := c.path() + ".tmp"
		if err = os.WriteFile(tmp, b, 0o644); err == nil {
			err = os.Rename(tmp, c.path())
		}
	}
	if err != nil {
		c.a.Store.Event("", EventCIError, "save CI state: "+err.Error())
	}
}
