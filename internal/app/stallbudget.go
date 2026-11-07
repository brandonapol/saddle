package app

import (
	"context"
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
	"github.com/brandonapol/saddle/internal/forge"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// The stall budget (#406): a blocker costs one PR, not the night. Every
// decision card carries a budget ([stall] budget, per kind under
// [stall.kinds]); when it runs out with no owner answer, StallTick runs the
// card's default option, the registered action it names (defaultRuns),
// never a destructive one:
//
//   - a stuck stack layer leaves the stack as its own standalone PR, the
//     layers above that don't build on it restack onto base and keep
//     merging, and the ones that do stay held with it until it merges; one
//     that can't stand alone cuts the stack instead, waiting off it until
//     the layers below merge (#766);
//   - a CI repair cap drops the red layer from the stack the same way, and
//     its own PR, only that one, gets needs-human (#769);
//   - a worker sitting on a risky prompt gets Escape and is told to find
//     another way; the same prompt unanswered a second time parks it
//     (paused, window closed, slot freed) and it resumes by itself after
//     another wait;
//   - a worker left in needs-you past [stall] hung_after gets a card with the
//     same deny-then-park default (prompt_wait.go, #786);
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
	// HungAfter is how long a running worker may make no progress before
	// the hung-worker watch acts, and how long each recovery step waits
	// (#625). A negative value turns the watch off.
	HungAfter time.Duration `toml:"hung_after"`
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
		if f.Stall.HungAfter != 0 {
			c.HungAfter = f.Stall.HungAfter
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
	if c.HungAfter == 0 {
		c.HungAfter = DefaultHungAfter
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
// due (#464). It also retries every pending repair spawn (#768). The
// sentinel runs it every cycle, with the stuck-stack alarm. It returns the cards it closed.
func (a *App) StallTick(now time.Time) ([]DecisionCard, error) {
	a.RetryRepairs()
	pre, err := a.ApplyPreauth(now)
	errs := []error{err}
	done, err := a.ExpireDecisions(now, a.runDefault)
	done = append(pre, done...)
	errs = append(errs, err, a.releaseHeld(now), a.resumeParked(now))
	if _, err := a.DecisionDigestTick(now); err != nil {
		errs = append(errs, fmt.Errorf("decisions digest: %w", err))
	}
	if _, err := a.OrphanTick(now); err != nil {
		errs = append(errs, fmt.Errorf("done-orphan watch: %w", err))
	}
	if _, err := a.DonePendingEnvTick(now); err != nil {
		errs = append(errs, fmt.Errorf("done-pending-env retry: %w", err))
	}
	if _, err := a.HungTick(now); err != nil {
		errs = append(errs, fmt.Errorf("hung-worker watch: %w", err))
	}
	if _, err := a.PromptWaitTick(now); err != nil {
		errs = append(errs, fmt.Errorf("prompt-wait watch: %w", err))
	}
	if _, _, err := a.WatchdogTick(now); err != nil {
		errs = append(errs, fmt.Errorf("watchdog: %w", err))
	}
	return done, errors.Join(errs...)
}

// Registered default actions (#790). A card's default option names one in
// its Run field, and runDefault runs exactly that, so what the card says
// happens is what happens. No action may leave a task or layer in a state
// whose only exit is a human command.
const (
	RunNothing       = "nothing"        // waiting is the default; the watch that opened the card goes on
	RunRouteAround   = "route-around"   // take the stuck layer out of the stack (RouteAroundStuck)
	RunDropRed       = "drop-red"       // label the red PR needs-human and route around it (#769)
	RunDenyPrompt    = "deny-prompt"    // Escape the prompt and tell the agent to find another way
	RunMessageTask   = "message-task"   // send the task the quoted message in the action
	RunOwnerDecision = "owner-decision" // tell the blocked task the option was taken
	RunRescueStalled = "rescue-stalled" // rescue the zombies and given-up tasks a stall names (#783)
	RunResumeLost    = "resume-lost"    // give the windowless tasks a stall names new windows (#783)
)

// defaultRuns maps each registered action to what it does.
var defaultRuns = map[string]func(a *App, c DecisionCard, o DecisionOption) error{
	RunNothing:        func(*App, DecisionCard, DecisionOption) error { return nil },
	RunRouteAround:    func(a *App, c DecisionCard, _ DecisionOption) error { return a.RouteAroundStuck(c.Task, c.Timeout) },
	RunDropRed:        (*App).dropRed,
	"park-repair-cap": (*App).dropRed, // a card saved before #769
	RunDenyPrompt:     (*App).denyPrompt,
	RunMessageTask: func(a *App, c DecisionCard, o DecisionOption) error {
		return a.Notify(c.Task, store.NoticeAction, quotedMessage(o.Action))
	},
	RunOwnerDecision: func(a *App, c DecisionCard, _ DecisionOption) error {
		return a.applyOwnerDecision(c, c.Default, DecidedByTimeout, "")
	},
	RunRescueStalled: (*App).rescueStalled,
	RunResumeLost:    (*App).resumeStalled,
}

// legacyRuns is the action for a card saved before options named theirs.
var legacyRuns = map[string]string{
	DecisionStuckStack:  RunRouteAround,
	DecisionCIRepairCap: RunDropRed,
	DecisionRiskyPrompt: RunDenyPrompt,
	DecisionAskOwner:    RunMessageTask,
	DecisionOwner:       RunOwnerDecision,
}

// runDefault runs a timed-out card's default option: the registered action
// the option names.
func (a *App) runDefault(c DecisionCard, o DecisionOption) error {
	run := o.Run
	if run == "" {
		run = legacyRuns[c.Kind]
	}
	if run == "" {
		return nil // the watchdog's card and unknown kinds: waiting is the default
	}
	fn, ok := defaultRuns[run]
	if !ok {
		return fmt.Errorf("default option %q runs %q, which saddle doesn't know", o.Label, run)
	}
	return fn(a, c, o)
}

// quotedMessage is the message in an action like `saddle message t5 "…"`.
func quotedMessage(action string) string {
	if i := strings.Index(action, `"`); i >= 0 {
		return strings.Trim(action[i:], `"`)
	}
	return "No answer from the owner: go ahead on your best guess and say so in your done summary."
}

// deniedNote is what a worker whose risky prompt was denied is told.
const deniedNote = "Denied: find another way, or explain why you need it."

// denyPrompt is the risky-prompt default: Escape the prompt and tell the
// agent to find another way, as the card says. The same prompt timing out
// a second time parks the task, with its resume scheduled after the same
// wait (#790).
func (a *App) denyPrompt(c DecisionCard, o DecisionOption) error {
	t, err := a.Store.Task(c.Task)
	if err != nil {
		return err
	}
	if !liveStatus(t.Status) {
		return nil // it moved on by itself
	}
	if a.timedOutBefore(c) {
		return a.ParkPrompt(c.Task, c.Timeout, c.Opened.Add(2*c.Timeout))
	}
	if a.ownWindow(t) {
		if err := a.SendKeys(c.Task, "", []string{"Escape"}); err != nil {
			return err
		}
	}
	a.Store.Event(c.Task, "stall_denied", "prompt unanswered")
	return a.Notify(c.Task, store.NoticeAction, quotedMessage(o.Action))
}

// timedOutBefore reports whether a card with c's ID already ran its
// default on a timeout.
func (a *App) timedOutBefore(c DecisionCard) bool {
	cs, _ := a.Decisions()
	return slices.ContainsFunc(cs, func(o DecisionCard) bool {
		return o.ID == c.ID && !o.Open() && o.By == DecidedByTimeout
	})
}

// heldState is the layers held on a red layer saddle routed around, saved
// under .saddle.
type heldState struct {
	Routes []StallRoute `json:"routes,omitempty"`
}

// StallRoute is one route-around: the red layer that left the stack and the
// layers above it that build on it, held with it until it merges. A
// truncated route (#766) has no PR: the red layer couldn't stand alone, so
// it and Held wait off the stack until the layers Below it merge, then go
// back on top.
type StallRoute struct {
	Red   string    `json:"red"`
	PR    string    `json:"pr"`
	Held  []string  `json:"held,omitempty"`
	Below []string  `json:"below,omitempty"`
	At    time.Time `json:"at"`
}

// stallRouteMaxAge is how long a route-around may hold layers before saddle
// gives up on it: it drops the route and files one blocker issue (#776).
const stallRouteMaxAge = 24 * time.Hour

// eventStallRouteExpired is written when a route outlives stallRouteMaxAge;
// the blocker filer turns it into an issue. Data: the held layers.
const eventStallRouteExpired = "stall_route_expired"

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
// waited after. The layers above it that build on it (they run after it, or
// change files it or they change) leave with it and stay held, and the rest
// restack onto base and keep merging. The red layer stands alone as its own
// PR against base when it can: its existing PR when that already targets
// base, else a fresh one through publish. When it can't (it needs work from
// the layers below it, or it fails the pre-publish gate by itself, which is
// why it is stuck) the stack is truncated at it instead (#766): it waits
// off the stack with no PR, the layers below keep merging, and once they
// have it goes back on top (releaseHeld). A layer already out of the stack
// is done.
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
	var below []string
	for _, l := range stack[:at] {
		if stackOf[l.ID] == stackOf[red] {
			below = append(below, l.ID)
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
			if forge.Transient(err) {
				return fmt.Errorf("route around %s: %w", red, err)
			}
			// It can't stand alone: truncate the stack at it.
			a.Store.Event(red, "stall_truncate", oneLine(err.Error()))
			url = ""
		} else {
			url = res.URL
		}
	}
	if url != "" {
		below = nil // it stands alone, so it waits on nothing below
	}

	unlock, err = a.lockTrain()
	if err != nil {
		return err
	}
	why := fmt.Sprintf("routed around after %s with no answer; it waits as the standalone PR %s", fmtDuration(after), url)
	if url == "" {
		why = fmt.Sprintf("routed around after %s with no answer; it can't stand alone, so it waits off the stack until %s merge", fmtDuration(after), orNothing(below))
	}
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
		StallRoute{Red: red, PR: url, Held: dependent, Below: below, At: time.Now().UTC()})
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
	return a.Notify(OrchestratorID, store.NoticeAction, routeLine(red, url, after, independent, dependent, below, a.Cfg.Base))
}

func orNothing(ids []string) string {
	if len(ids) == 0 {
		return "nothing"
	}
	return strings.Join(ids, ", ")
}

// routeLine is the one chat line a route-around leaves.
func routeLine(red, url string, after time.Duration, went, held, below []string, base string) string {
	s := fmt.Sprintf("Routed around %s after %s. It's now a standalone PR %s waiting on you", red, fmtDuration(after), url)
	if url == "" {
		s = fmt.Sprintf("Routed around %s after %s. It can't stand alone as a PR, so the stack is cut at it", red, fmtDuration(after))
		if len(below) > 0 {
			s += fmt.Sprintf(": %s keep merging, and %s goes back on top once they have", strings.Join(below, ", "), red)
		}
	}
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
// train once base has the red layer's work. A truncated route (no PR) puts
// the red layer itself back first, on top, once base has the layers that
// were below it (#766). A red layer that will never merge (its PR closed,
// its task killed or no_change) releases the layers held on it to rebuild
// on base without its work; one that can't replay gets the train's usual
// conflict repair. A route still unresolved after stallRouteMaxAge is
// dropped with one blocker issue (#776).
func (a *App) releaseHeld(now time.Time) error {
	routes, err := a.StallHeld()
	if err != nil || len(routes) == 0 {
		return err
	}
	all, err := a.landedAll()
	if err != nil {
		return err
	}
	base := "refs/remotes/origin/" + a.Cfg.Base
	merged := func(id string) bool {
		i := slices.IndexFunc(all, func(l landedTask) bool { return l.ID == id })
		return i >= 0 && (all[i].State == TrainMerged || a.inBase(all[i], base))
	}
	var keep []StallRoute
	var errs []error
	for _, r := range routes {
		var back []string
		gone := ""
		switch {
		case r.PR == "" && !slices.ContainsFunc(r.Below, func(id string) bool { return !merged(id) }):
			back = append([]string{r.Red}, r.Held...)
		case r.PR != "" && merged(r.Red):
			back = r.Held
		default:
			if gone = a.redGone(r); gone != "" {
				back = r.Held
				break
			}
			if !r.At.IsZero() && now.Sub(r.At) > stallRouteMaxAge {
				errs = append(errs, a.expireRoute(r))
				continue
			}
			keep = append(keep, r)
			continue
		}
		for _, id := range back {
			if err := a.Requeue(id); err != nil {
				errs = append(errs, fmt.Errorf("requeue %s held on %s: %w", id, r.Red, err))
			}
		}
		switch {
		case gone != "":
			a.Store.Event(r.Red, "stall_route_released", gone+" held="+strings.Join(back, ","))
			if len(back) > 0 {
				errs = append(errs, a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf(
					"%s %s, so it won't merge: %s, held on it, %s back in the train to rebuild on %s without its work.",
					r.Red, gone, strings.Join(back, ", "), isAre(len(back)), a.Cfg.Base)))
			}
		case r.PR == "":
			errs = append(errs, a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf(
				"The layers below %s merged, so %s %s back on top of the stack.", r.Red, strings.Join(back, ", "), isAre(len(back)))))
		case len(back) > 0:
			errs = append(errs, a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf(
				"%s merged, so %s, held on it, are back in the train.", r.Red, strings.Join(back, ", "))))
		}
	}
	errs = append(errs, a.setStallHeld(keep))
	return errors.Join(errs...)
}

// redGone says why a routed-around red layer will never merge: its task
// was killed or ended with no change, or its PR was closed unmerged. It
// returns "" while it still might, or when the forge can't tell.
func (a *App) redGone(r StallRoute) string {
	if t, err := a.Store.Task(r.Red); err == nil {
		switch t.Status {
		case store.Killed:
			return "was killed"
		case store.NoChange:
			return "ended with no change"
		}
	}
	if r.PR == "" {
		return ""
	}
	if pr, err := a.ghLookup(r.PR); err == nil && pr.State == forge.StateClosed {
		return "was closed (its PR " + r.PR + ")"
	}
	return ""
}

// expireRoute gives up on a route that outlived stallRouteMaxAge: the
// event becomes one blocker issue and the orchestrator hears how to put
// the held layers back by hand.
func (a *App) expireRoute(r StallRoute) error {
	held := append([]string{}, r.Held...)
	if r.PR == "" {
		held = append([]string{r.Red}, held...)
	}
	a.Store.Event(r.Red, eventStallRouteExpired, strings.Join(held, ","))
	msg := fmt.Sprintf("Gave up on the route around %s after %s: it neither merged nor closed.", r.Red, fmtDuration(stallRouteMaxAge))
	if len(held) > 0 {
		cmds := make([]string, len(held))
		for i, id := range held {
			cmds[i] = "saddle requeue " + id
		}
		msg += fmt.Sprintf(" %s %s still off the stack; %s puts them back.", strings.Join(held, ", "), isAre(len(held)), strings.Join(cmds, " and "))
	}
	return a.Notify(OrchestratorID, store.NoticeAction, msg)
}

// dropRed is the ci-repair-cap default, as its card says: the red layer
// leaves the stack so the layers above keep merging, and its own PR gets
// needs-human. The label is a courtesy: a failure must not stop the drop,
// or the default would fail and retry every cycle (#787).
func (a *App) dropRed(c DecisionCard, _ DecisionOption) error {
	if t, err := a.Store.Task(c.Task); err == nil && t.PR != "" {
		if err := a.Forge().AddLabel(context.Background(), t.PR, automerge.NeedsHuman); err != nil {
			a.Store.Event(c.Task, "stall_label_failed", fmt.Sprintf("label %s %s: %v", t.PR, automerge.NeedsHuman, err))
		}
	}
	return a.RouteAroundStuck(c.Task, c.Timeout)
}

// ParkRepairCap parks a red layer whose repairs ran out: its PR gets the
// needs-human label and it stops holding the stack, so everything else
// moves on. It is no card's default any more (dropRed is, #769); the
// labels journey still drives it by hand.
func (a *App) ParkRepairCap(task string, after time.Duration) error {
	t, err := a.Store.Task(task)
	if err != nil {
		return err
	}
	if t.PR != "" {
		// The label is a courtesy: a failure must not stop the park, or the
		// default would fail and retry every cycle (#787).
		if err := a.Forge().AddLabel(context.Background(), t.PR, automerge.NeedsHuman); err != nil {
			a.Store.Event(task, "stall_label_failed", fmt.Sprintf("label %s %s: %v", t.PR, automerge.NeedsHuman, err))
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

// StallPark is a worker the stall budget parked, and when it resumes.
type StallPark struct {
	Task     string    `json:"task"`
	ResumeAt time.Time `json:"resume_at"`
}

func (a *App) parkedPath() string { return a.stateDir("stall-parked.json") }

// StallParked lists the parked workers waiting on their scheduled resume.
func (a *App) StallParked() ([]StallPark, error) {
	var ps []StallPark
	b, err := os.ReadFile(a.parkedPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &ps); err != nil {
		return nil, fmt.Errorf("%s: %w", a.parkedPath(), err)
	}
	return ps, nil
}

func (a *App) setStallParked(ps []StallPark) error {
	b, err := json.MarshalIndent(ps, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.parkedPath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.parkedPath())
}

// resumeParked resumes each parked worker whose cooldown ran out at now.
// One no longer paused (resumed or killed by hand) is just dropped; one
// that fails to resume is dropped too, with an event and a notice, so it
// never retries forever.
func (a *App) resumeParked(now time.Time) error {
	ps, err := a.StallParked()
	if err != nil || len(ps) == 0 {
		return err
	}
	var keep []StallPark
	var errs []error
	for _, p := range ps {
		if now.Before(p.ResumeAt) {
			keep = append(keep, p)
			continue
		}
		t, err := a.Store.Task(p.Task)
		if err != nil || t.Status != StatusPaused {
			continue
		}
		if _, err := a.Resume(p.Task); err != nil {
			a.Store.Event(p.Task, "stall_resume_failed", err.Error())
			errs = append(errs, a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
				"Could not resume %s, parked on an unanswered prompt: %v. saddle resume %s or saddle kill %s.", p.Task, err, p.Task, p.Task)))
			continue
		}
		a.Store.Event(p.Task, "stall_resumed", "after park")
	}
	errs = append(errs, a.setStallParked(keep))
	return errors.Join(errs...)
}

// ParkPrompt parks a worker sitting on a prompt nobody answered twice: its
// uncommitted work is snapshotted, it is marked paused and its window
// closes, which frees the slot for the next ready ticket. It resumes by
// itself at resumeAt (resumeParked); saddle resume <task> picks it up
// sooner.
func (a *App) ParkPrompt(task string, after time.Duration, resumeAt time.Time) error {
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
	ps, err := a.StallParked()
	if err != nil {
		return err
	}
	ps = append(slices.DeleteFunc(ps, func(p StallPark) bool { return p.Task == task }), StallPark{Task: task, ResumeAt: resumeAt.UTC()})
	if err := a.setStallParked(ps); err != nil {
		return err
	}
	a.Store.Event(task, "stall_parked", "prompt unanswered")
	return a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
		"Parked %s after a prompt went unanswered twice; its slot is free. It resumes by itself after %s, or saddle resume %s now.", task, fmtDuration(after), task))
}
