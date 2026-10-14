package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/brandonapol/saddle/internal/automerge"
	"github.com/brandonapol/saddle/internal/claims"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// The stall budget (#406): a blocker costs one PR, not the night. Every
// decision card carries a budget ([stall] budget, per kind under
// [stall.kinds]); when it runs out with no owner answer, StallTick runs the
// card's route-around default, never a destructive one:
//
//   - a stuck stack layer leaves the stack as its own standalone PR, the
//     layers above that don't build on it restack onto base and keep
//     merging, and the ones that do stay held with it until it merges;
//   - a CI repair cap parks the red layer: its PR gets needs-human and it
//     stops holding the stack;
//   - a worker sitting on a prompt is parked (paused, window closed), which
//     frees its slot;
//   - an unanswered ask_owner tells the asker to go ahead on its best guess.
//
// Each route-around leaves one line in chat, as an action notice: what it
// left waits on the owner. The no-progress watchdog lives
// in watchdog.go.

// Card kinds the stall budget adds.
const DecisionNoProgress = "no-progress"

// DefaultIdleAlarm is how long nothing may land, merge or start while work
// waits before the watchdog posts its card.
const DefaultIdleAlarm = 45 * time.Minute

// StallConfig is the [stall] config table.
type StallConfig struct {
	// Budget is how long any card waits for an answer before its default runs.
	Budget time.Duration `toml:"budget"`
	// Kinds overrides Budget per card kind, as in `stuck-stack = "1h"`.
	Kinds map[string]time.Duration `toml:"kinds"`
	// IdleAlarm is the no-progress watchdog's threshold.
	IdleAlarm time.Duration `toml:"idle_alarm"`
	// DoneOrphan is how long a worker whose done call was cancelled may sit
	// unqueued with a still screen before the watchdog heals it, and how
	// long each healing step waits (#467).
	DoneOrphan time.Duration `toml:"done_orphan"`
}

// set is the budget config sets for kind, if any.
func (c StallConfig) set(kind string) (time.Duration, bool) {
	if d := c.Kinds[kind]; d > 0 {
		return d, true
	}
	return c.Budget, c.Budget > 0
}

// For is kind's budget.
func (c StallConfig) For(kind string) time.Duration {
	if d, ok := c.set(kind); ok {
		return d
	}
	return DefaultDecisionTimeout
}

// StallConfig reads [stall] from the user's and the repo's config files, the
// repo's winning, over the defaults.
func (a *App) StallConfig() StallConfig {
	c := StallConfig{Kinds: map[string]time.Duration{}}
	paths := []string{a.stateDir("config.toml")}
	if home, err := os.UserConfigDir(); err == nil {
		paths = append([]string{filepath.Join(home, "saddle", "config.toml")}, paths...)
	}
	for _, p := range paths {
		var f struct {
			Stall StallConfig `toml:"stall"`
		}
		if _, err := toml.DecodeFile(p, &f); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				a.Store.Event("", "stall_config_error", err.Error())
			}
			continue
		}
		if f.Stall.Budget > 0 {
			c.Budget = f.Stall.Budget
		}
		if f.Stall.IdleAlarm > 0 {
			c.IdleAlarm = f.Stall.IdleAlarm
		}
		if f.Stall.DoneOrphan > 0 {
			c.DoneOrphan = f.Stall.DoneOrphan
		}
		for k, d := range f.Stall.Kinds {
			c.Kinds[k] = d
		}
	}
	if c.IdleAlarm <= 0 {
		c.IdleAlarm = DefaultIdleAlarm
	}
	if c.DoneOrphan <= 0 {
		c.DoneOrphan = DefaultDoneOrphan
	}
	return c
}

// StallHold is one thing holding work back, with when it started and how
// long it may before saddle routes around it.
type StallHold struct {
	Kind   string        `json:"kind"`
	Task   string        `json:"task"`
	What   string        `json:"what"`
	Card   string        `json:"card,omitempty"` // its decision card, once it has one
	Since  time.Time     `json:"since"`
	Budget time.Duration `json:"budget"`
}

// Left is how much of its budget is left at now.
func (h StallHold) Left(now time.Time) time.Duration {
	return max(0, h.Since.Add(h.Budget).Sub(now))
}

// StallHolds lists every open card and every red or conflicting layer, each
// with since and budget. A layer with an open card is listed once, as its
// card.
func (a *App) StallHolds(now time.Time) ([]StallHold, error) {
	open, err := a.OpenDecisions()
	if err != nil {
		return nil, err
	}
	var out []StallHold
	carded := map[string]bool{}
	for _, c := range open {
		out = append(out, StallHold{Kind: c.Kind, Task: c.Task, What: c.Headline, Card: c.ID, Since: c.Opened, Budget: c.Timeout})
		if c.Kind == DecisionStuckStack {
			carded[c.Task] = true
		}
	}
	layers, err := a.stuckLayers()
	if err != nil {
		return out, err
	}
	budget := a.Cfg.Train.StuckAfter + a.StallConfig().For(DecisionStuckStack)
	for _, l := range layers {
		if carded[l.Layer] {
			continue
		}
		since := l.Since
		if since.IsZero() {
			since = now
		}
		out = append(out, StallHold{Kind: DecisionStuckStack, Task: l.Layer, What: l.Detail, Since: since, Budget: budget})
	}
	return out, nil
}

// stallBudget is the timeout config sets for a card of kind, if it sets one.
func (a *App) stallBudget(kind string) (time.Duration, bool) {
	return a.StallConfig().set(kind)
}

// StallTick runs the stall budget at now: it closes the cards a
// [[preauth]] rule allows, runs the default of every reversible card whose
// budget ran out, puts back layers held on a red layer that merged, runs
// the no-progress watchdog and sends the day's decisions digest once it is
// due (#464). The sentinel runs it every cycle, with the stuck-stack alarm. It returns the cards it closed.
func (a *App) StallTick(now time.Time) ([]DecisionCard, error) {
	pre, err := a.ApplyPreauth(now)
	errs := []error{err}
	done, err := a.ExpireDecisions(now, a.runDefault)
	done = append(pre, done...)
	errs = append(errs, err, a.releaseHeld())
	if _, err := a.DecisionDigestTick(now); err != nil {
		errs = append(errs, fmt.Errorf("decisions digest: %w", err))
	}
	if _, err := a.OrphanTick(now); err != nil {
		errs = append(errs, fmt.Errorf("done-orphan watch: %w", err))
	}
	if _, _, err := a.WatchdogTick(now); err != nil {
		errs = append(errs, fmt.Errorf("watchdog: %w", err))
	}
	return done, errors.Join(errs...)
}

// runDefault runs a timed-out card's route-around default.
func (a *App) runDefault(c DecisionCard, o DecisionOption) error {
	switch c.Kind {
	case DecisionStuckStack:
		return a.RouteAroundStuck(c.Task, c.Timeout)
	case DecisionCIRepairCap:
		return a.ParkRepairCap(c.Task, c.Timeout)
	case DecisionRiskyPrompt:
		return a.ParkPrompt(c.Task, c.Timeout)
	case DecisionAskOwner:
		msg := "No answer from the owner: go ahead on your best guess and say so in your done summary."
		if i := strings.Index(o.Action, `"`); i >= 0 {
			msg = strings.Trim(o.Action[i:], `"`)
		}
		return a.Notify(c.Task, store.NoticeAction, msg)
	case DecisionOwner:
		return a.applyOwnerDecision(c, c.Default, DecidedByTimeout, "")
	}
	return nil // the watchdog's card and unknown kinds: waiting is the default
}

// heldState is the layers held on a red layer saddle routed around, saved
// under .saddle.
type heldState struct {
	Routes []StallRoute `json:"routes,omitempty"`
}

// StallRoute is one route-around: the red layer that left the stack and the
// layers above it that build on it, held with it until it merges.
type StallRoute struct {
	Red  string    `json:"red"`
	PR   string    `json:"pr"`
	Held []string  `json:"held,omitempty"`
	At   time.Time `json:"at"`
}

func (a *App) heldPath() string { return a.stateDir("stall-held.json") }

// StallHeld lists the route-arounds whose red layer hasn't merged yet.
func (a *App) StallHeld() ([]StallRoute, error) {
	var s heldState
	b, err := os.ReadFile(a.heldPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", a.heldPath(), err)
	}
	return s.Routes, nil
}

func (a *App) setStallHeld(rs []StallRoute) error {
	b, err := json.MarshalIndent(heldState{Routes: rs}, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.heldPath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.heldPath())
}

// RouteAroundStuck takes the red layer out of its PR stack after its card
// waited after: it stands alone as its own PR against base (its existing PR
// when that already targets base, else a fresh one through publish), the
// layers above it that build on it (they run after it, or change files it
// or they change) leave with it and stay held, and the rest restack onto
// base and keep merging. A layer already out of the stack is done.
func (a *App) RouteAroundStuck(red string, after time.Duration) error {
	unlock, err := a.lockTrain()
	if err != nil {
		return err
	}
	stack, err := a.landedStack()
	if err != nil {
		unlock()
		return err
	}
	at := slices.IndexFunc(stack, func(l landedTask) bool { return l.ID == red })
	if at < 0 {
		unlock()
		return nil
	}
	stackOf, above, err := a.stackNames()
	unlock()
	if err != nil {
		return err
	}
	dependent := a.dependentLayers(stack, at, above[red])
	var independent []string
	for _, id := range above[red] {
		if !slices.Contains(dependent, id) {
			independent = append(independent, id)
		}
	}

	t := stack[at].Task
	url := t.PR
	if url == "" || stackOf[red] != red {
		// Its PR sits on a layer below: publish its own work against base.
		if err := a.Store.SetField(red, "pr", ""); err != nil {
			return err
		}
		res, err := a.Publish(PublishReq{Target: red})
		if err != nil {
			_ = a.Store.SetField(red, "pr", t.PR)
			return fmt.Errorf("route around %s: %w", red, err)
		}
		url = res.URL
	}

	unlock, err = a.lockTrain()
	if err != nil {
		return err
	}
	why := fmt.Sprintf("routed around after %s with no answer; it waits as the standalone PR %s", fmtDuration(after), url)
	err = a.leaveStack(stack[at], TrainSuperseded, why)
	for _, id := range dependent {
		i := slices.IndexFunc(stack, func(l landedTask) bool { return l.ID == id })
		if err == nil && i >= 0 {
			err = a.leaveStack(stack[i], TrainSuperseded, "held with "+red+", which it builds on, until "+red+" merges")
		}
	}
	unlock()
	if err != nil {
		return err
	}
	routes, err := a.StallHeld()
	if err != nil {
		return err
	}
	routes = append(slices.DeleteFunc(routes, func(r StallRoute) bool { return r.Red == red }),
		StallRoute{Red: red, PR: url, Held: dependent, At: time.Now().UTC()})
	if err := a.setStallHeld(routes); err != nil {
		return err
	}
	// It left the stack, so its red CI holds nothing now (as the ci-red
	// watcher's next poll would find).
	if s, err := a.CIRed(); err == nil {
		s.Red = slices.DeleteFunc(s.Red, func(l CIRedLayer) bool { return l.Task == red })
		_ = a.SetCIRed(s)
	}
	a.Store.Event(red, "stall_routed", url+" held="+strings.Join(dependent, ",")+" after="+fmtDuration(after))
	if _, err := a.Restack(); err != nil {
		return err
	}
	return a.Notify(OrchestratorID, store.NoticeAction, routeLine(red, url, after, independent, dependent, a.Cfg.Base))
}

// routeLine is the one chat line a route-around leaves.
func routeLine(red, url string, after time.Duration, went, held []string, base string) string {
	s := fmt.Sprintf("Routed around %s after %s. It's now a standalone PR %s waiting on you", red, fmtDuration(after), url)
	if len(went) > 0 {
		s += fmt.Sprintf("; %s restacked onto %s and keep merging without it", strings.Join(went, ", "), base)
	}
	if len(held) > 0 {
		s += fmt.Sprintf("; %s build on it, so they stay held until it merges", strings.Join(held, ", "))
	}
	return s + "."
}

// dependentLayers lists the layers of ups (above stack[at]) that build on
// stack[at]: they run after it or a layer that does, were spawned by one, or
// change a file one of those changes. Serial files don't count.
func (a *App) dependentLayers(stack []landedTask, at int, ups []string) []string {
	files := func(l landedTask) []string {
		if l.From == "" || l.Lost != "" {
			return nil
		}
		fs, _ := gitx.ChangedFiles(a.Root, l.From, l.To)
		return slices.DeleteFunc(fs, func(f string) bool {
			return slices.ContainsFunc(a.Cfg.Serial, func(g string) bool { return claims.Match(g, f) })
		})
	}
	on := []string{stack[at].ID}
	touched := map[string]bool{}
	for _, f := range files(stack[at]) {
		touched[f] = true
	}
	var out []string
	for _, l := range stack[at+1:] {
		if !slices.Contains(ups, l.ID) {
			continue
		}
		fs := files(l)
		dep := slices.Contains(on, l.Parent) || slices.ContainsFunc(a.TaskAfter(l.ID), func(d string) bool { return slices.Contains(on, d) }) ||
			slices.ContainsFunc(fs, func(f string) bool { return touched[f] })
		if !dep {
			continue
		}
		out = append(out, l.ID)
		on = append(on, l.ID)
		for _, f := range fs {
			touched[f] = true
		}
	}
	return out
}

// releaseHeld puts layers held on a routed-around red layer back in the
// train once base has the red layer's work.
func (a *App) releaseHeld() error {
	routes, err := a.StallHeld()
	if err != nil || len(routes) == 0 {
		return err
	}
	all, err := a.landedAll()
	if err != nil {
		return err
	}
	base := "refs/remotes/origin/" + a.Cfg.Base
	var keep []StallRoute
	var errs []error
	for _, r := range routes {
		i := slices.IndexFunc(all, func(l landedTask) bool { return l.ID == r.Red })
		if i < 0 || !a.inBase(all[i], base) {
			keep = append(keep, r)
			continue
		}
		for _, id := range r.Held {
			if err := a.Requeue(id); err != nil {
				errs = append(errs, fmt.Errorf("requeue %s held on %s: %w", id, r.Red, err))
			}
		}
		if len(r.Held) > 0 {
			errs = append(errs, a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf(
				"%s merged, so %s, held on it, are back in the train.", r.Red, strings.Join(r.Held, ", "))))
		}
	}
	errs = append(errs, a.setStallHeld(keep))
	return errors.Join(errs...)
}

// ParkRepairCap parks a red layer whose repairs ran out: its PR gets the
// needs-human label and it stops holding the stack, so everything else
// moves on.
func (a *App) ParkRepairCap(task string, after time.Duration) error {
	t, err := a.Store.Task(task)
	if err != nil {
		return err
	}
	if t.PR != "" {
		if _, err := gh(a.Root, "pr", "edit", t.PR, "--add-label", automerge.NeedsHuman); err != nil {
			return fmt.Errorf("label %s %s: %w", t.PR, automerge.NeedsHuman, err)
		}
	}
	s, err := a.CIRed()
	if err != nil {
		return err
	}
	for i, l := range s.Red {
		if l.Task == task {
			s.Red[i].Acked = true
		}
	}
	if err := a.SetCIRed(s); err != nil {
		return err
	}
	a.Store.Event(task, "stall_parked", "repair cap: "+automerge.NeedsHuman)
	return a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
		"Parked %s after %s with no answer: its PR %s is labeled %s and no longer holds the stack.", task, fmtDuration(after), t.PR, automerge.NeedsHuman))
}

// ParkPrompt parks a worker sitting on a prompt nobody answered: its
// uncommitted work is snapshotted, it is marked paused and its window
// closes, which frees the slot for the next ready ticket. `saddle resume
// <task>` picks it up again.
func (a *App) ParkPrompt(task string, after time.Duration) error {
	t, err := a.Store.Task(task)
	if err != nil {
		return err
	}
	if !liveStatus(t.Status) {
		return nil // it moved on by itself
	}
	a.snapshotWIP(t.ID, t.Worktree)
	if err := a.Store.SetStatus(t.ID, StatusPaused); err != nil {
		return err
	}
	if t.Window != "" && a.Tmux.Alive(t.Window) {
		if err := a.Tmux.KillWindow(t.Window); err != nil {
			return err
		}
	}
	a.Store.Event(task, "stall_parked", "prompt unanswered")
	return a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
		"Parked %s after %s on a prompt nobody answered; its slot is free. saddle resume %s picks it up again.", task, fmtDuration(after), task))
}
