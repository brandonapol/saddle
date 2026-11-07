// Package sentinel watches the PR stack for the things that break it: a PR
// GitHub can't merge, base moving past integration, and branches that drifted
// or no longer show only their task's work. People use GitHub too (#119):
// tasks whose PR was merged or closed, and killed tasks, leave the stack for
// good before anything is checked, so they never flag it. On a hit the
// sentinel records one stack_at_risk event, tells the orchestrator (and so
// the TUI) to run restack, and flags the stack so prs and land hold back the
// work that depends on the broken layers. Only a conflict, which needs a
// human decision, labels PRs needs-human; each labeled PR gets one comment
// saying which conflict, kept current as the cause changes and turned into a
// note when the label lifts. When everything it finds is drift a restack
// fixes (base moved, a layer left the stack with its commits still on
// integration), it restacks itself instead of flagging, with backoff and at
// most three tries per flagged task, and flags only when that fails (#775).
// It never restacks over a conflict. Once the stack checks clean it lifts the
// labels and the flag. `saddle sentinel ack` and
// `saddle unstack` are the escape hatches when restack can't fix it.
package sentinel

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/forge"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// Label marks PRs a human must look at before they merge.
const Label = "needs-human"

// Event kinds in the events table.
const (
	EventAtRisk = "stack_at_risk"
	EventClear  = "stack_clear"
	EventError  = "sentinel_error"
)

// DefaultInterval is how often Run checks the stack.
const DefaultInterval = 2 * time.Minute

// DefaultBusyRetry is how soon Run checks again after a cycle found the train
// lock held.
const DefaultBusyRetry = 2 * time.Second

// PR is what GitHub says about a pull request.
type PR = app.PRInfo

// GitHub is the part of GitHub the sentinel uses.
type GitHub interface {
	PR(url string) (PR, error)
	AddLabel(url, label string) error
	RemoveLabel(url, label string) error
	Comments(url string) ([]Comment, error)
	AddComment(url, body string) error
	EditComment(id, body string) error
}

// Comment is a PR comment; ID is what EditComment takes.
type Comment struct {
	ID   string `json:"id"`
	Body string `json:"body"`
}

// GH is the sentinel's GitHub: the repo's forge. Labels and comments are
// optional: on a forge without them they do nothing, and the flag shows in
// saddle only.
type GH struct {
	Forge forge.Forge
	ready bool // the label exists
}

func (g *GH) PR(url string) (PR, error) {
	p, err := g.Forge.PR(context.Background(), url, forge.PRState|forge.PRMergeable|forge.PRBase)
	if err != nil {
		return PR{}, err
	}
	return PR{State: p.State, Mergeable: p.Mergeable, BaseRefName: p.Base}, nil
}

func (g *GH) AddLabel(url, label string) error {
	ctx := context.Background()
	if !g.ready {
		// Labeling a PR fails on a label the repo doesn't have yet.
		if err := g.Forge.EnsureLabel(ctx, forge.Label{Name: label, Color: "D93F0B",
			Description: "Saddle flagged this PR's stack; run restack before merging"}); err != nil {
			return optional(err)
		}
		g.ready = true
	}
	return optional(g.Forge.AddLabel(ctx, url, label))
}

func (g *GH) RemoveLabel(url, label string) error {
	return optional(g.Forge.RemoveLabel(context.Background(), url, label))
}

func (g *GH) Comments(url string) ([]Comment, error) {
	cs, err := g.Forge.Comments(context.Background(), url)
	if err != nil {
		return nil, optional(err)
	}
	out := make([]Comment, len(cs))
	for i, c := range cs {
		out[i] = Comment(c)
	}
	return out, nil
}

func (g *GH) AddComment(url, body string) error {
	return optional(g.Forge.AddComment(context.Background(), url, body))
}

func (g *GH) EditComment(id, body string) error {
	return optional(g.Forge.EditComment(context.Background(), id, body))
}

// optional drops forge.ErrUnsupported, for the operations a forge may lack.
// Provider is the forge's provider, so the sentinel's causes name it.
func (g *GH) Provider() string { return g.Forge.Provider() }

func optional(err error) error {
	if errors.Is(err, forge.ErrUnsupported) {
		return nil
	}
	return err
}

// Sentinel checks one repo's PR stack.
type Sentinel struct {
	App      *app.App
	GH       GitHub
	Interval time.Duration
	// BusyRetry is the wait after a cycle skipped because the train was busy.
	BusyRetry time.Duration
	// Blockers files a GitHub issue for each blocker escalated since the
	// last cycle (#407). Nil files none.
	Blockers *app.BlockerFiler
	// AutoRestack lets Check restack the stack itself when everything it
	// finds is drift a restack fixes (#775). New sets it; a restack's own
	// re-check (app.StackCheck) leaves it off so it never starts another.
	AutoRestack bool
	lastErr     string
	// posted is the sentinel comment last seen or written on each PR, so a
	// quiet tick costs no GitHub calls.
	posted map[string]Comment
	// restack rebuilds the stack; nil means App.Restack. Tests stub it.
	restack func() (app.RestackResult, error)
	now     func() time.Time
	// tries counts the sentinel's own restacks per flagged task, for the
	// backoff and the cap (#775). A clean check resets it.
	tries map[string]restackTry
}

// maxRestacks is how many times the sentinel restacks for one flagged task
// before it leaves the flag to a human.
const maxRestacks = 3

// Event kinds for the sentinel's own restacks.
const (
	EventRestack       = "sentinel_restack"
	EventRestackFailed = "sentinel_restack_failed"
)

type restackTry struct {
	n    int
	next time.Time
	err  string // why the last one failed, for the flag's cause
}

func init() {
	app.StackCheck = func(a *app.App) error {
		s := New(a)
		s.AutoRestack = false
		_, err := s.Check()
		return err
	}
}

// New returns a sentinel for a's stack that talks to the repo's forge.
func New(a *app.App) *Sentinel {
	return &Sentinel{App: a, GH: &GH{Forge: a.Forge()}, Interval: DefaultInterval, BusyRetry: DefaultBusyRetry, Blockers: a.NewBlockerFiler(), AutoRestack: true}
}

// Report is the outcome of one check.
type Report struct {
	Busy   bool     `json:"busy,omitempty"` // the train held its lock, so nothing was checked
	AtRisk bool     `json:"at_risk"`
	Acked  bool     `json:"acked,omitempty"` // someone acked this flag, so it freezes and labels nothing
	Task   string   `json:"task,omitempty"`  // the first broken task
	Cause  string   `json:"cause,omitempty"` // why it broke
	Hits   []string `json:"hits,omitempty"`  // every problem found, "task: problem"
	PRs    []string `json:"prs,omitempty"`   // PRs labeled needs-human
	// Restacked says the sentinel restacked the stack itself this check,
	// since everything it found was drift a restack fixes (#775).
	Restacked bool `json:"restacked,omitempty"`
	// Deferred says the sentinel's restack raced another push twice, so it
	// left the stack for the next cycle without flagging it.
	Deferred bool `json:"deferred,omitempty"`
}

// Run checks the stack now and then every Interval until ctx ends, opens
// the PRs of landed tasks that have none, and runs the stuck-stack alarm
// after each check. A failed
// check is recorded as an event (once per distinct error) and retried on the
// next tick. A cycle skipped because the train was busy is retried after
// BusyRetry instead.
func (s *Sentinel) Run(ctx context.Context) error {
	iv := s.Interval
	if iv <= 0 {
		iv = DefaultInterval
	}
	busy := s.BusyRetry
	if busy <= 0 {
		busy = DefaultBusyRetry
	}
	t := time.NewTimer(iv)
	defer t.Stop()
	for {
		wait := iv
		if s.cycle(ctx) {
			wait = min(busy, iv)
		}
		t.Reset(wait)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// cycle runs one round: the stack check, opening the PRs of landed tasks
// that have none (#819), the stuck-stack alarm, blocker issues and the
// invariant monitor (#825). It
// reports whether the train was busy, so the check was skipped.
func (s *Sentinel) cycle(ctx context.Context) (busy bool) {
	if rep, err := s.Check(); deferred(err) {
		// GitHub's quota is spent: skip the cycle quietly (#764).
	} else if err != nil {
		s.logErr(err.Error())
	} else {
		s.lastErr = ""
		busy = rep.Busy
	}
	// Landed means on the way to base: a landed task without a PR gets one
	// here, so a land whose publish failed, or one an older saddle ran,
	// heals by itself. Skipped while the train is busy; the next cycle does it.
	if !busy {
		if _, err := s.App.PublishLanded(app.PublishBySentinel); err != nil && !deferred(err) {
			s.logErr("opening landed tasks' PRs: " + err.Error())
		}
	}
	// A stack red or conflicting too long with nobody on it wakes the
	// orchestrator once (#223).
	if _, err := s.App.CheckStuck(time.Now().UTC()); err != nil && !deferred(err) {
		s.App.Store.Event("", EventError, "stuck-stack alarm: "+err.Error())
	}
	if s.Blockers != nil {
		if _, err := s.Blockers.FileBlockers(ctx); err != nil && !deferred(err) {
			s.App.Store.Event("", EventError, "blocker issues: "+err.Error())
		}
	}
	// Every ADR 0004 invariant violation is an event and one notice (#825).
	if _, err := s.App.CheckInvariants(time.Now().UTC()); err != nil && !deferred(err) {
		s.App.Store.Event("", EventError, "invariant monitor: "+err.Error())
	}
	return busy
}

// logErr records msg as an event, once per distinct message.
func (s *Sentinel) logErr(msg string) {
	if msg != s.lastErr {
		s.lastErr = msg
		s.App.Store.Event("", EventError, msg)
	}
}

// deferred reports whether err is GitHub's rate limit (#764) or a network
// call that timed out (#788), which a cycle waits out instead of logging.
func deferred(err error) bool { return forge.Transient(err) }

// hit is one problem, at index at of the stack's layers; the layers from
// from up are at risk. conflict marks a conflict, the one kind of hit that
// needs a human.
type hit struct {
	at, from int
	cause    string
	conflict bool
	fix      bool // a restack fixes it without a human
}

// Check runs one cycle: it takes finished tasks out of the stack, checks every
// layer left, then flags or clears the stack. It skips the cycle when the
// train is busy, since a land or restack in progress moves the refs it reads.
// When everything it finds is drift a restack fixes (base moved, a layer left
// the stack with its commits still on integration), it restacks itself and
// checks again, flagging only if that fails (#775). It backs off between its
// restacks for the same flagged task and stops after maxRestacks.
func (s *Sentinel) Check() (Report, error) {
	rep, fix, err := s.check(s.AutoRestack)
	if err != nil || !fix {
		return rep, err
	}
	s.tried(rep.Task)
	_, err = s.restackRetry()
	switch {
	case err == nil:
		s.App.Store.Event(rep.Task, EventRestack, rep.Cause)
		t := s.tries[rep.Task]
		t.err = ""
		s.tries[rep.Task] = t
		rep, _, err = s.check(false)
		rep.Restacked = true
		return rep, err
	case raced(err):
		// Automerge or a repair pushed while restack did: not a failure,
		// and not worth a flag. The next cycle restacks again.
		s.tries[rep.Task] = restackTry{n: s.tries[rep.Task].n - 1, next: s.clock(), err: s.tries[rep.Task].err}
		s.App.Store.Event(rep.Task, EventRestackFailed, "raced another push; retrying next cycle: "+err.Error())
		return Report{Deferred: true, Task: rep.Task, Cause: rep.Cause, Hits: rep.Hits}, nil
	default:
		s.App.Store.Event(rep.Task, EventRestackFailed, err.Error())
		t := s.tries[rep.Task]
		t.err = err.Error()
		s.tries[rep.Task] = t
		rep, _, err = s.check(false)
		return rep, err
	}
}

// restackRetry restacks; when the push lost a race ("stale info"), it fetches
// and tries once more.
func (s *Sentinel) restackRetry() (app.RestackResult, error) {
	run := s.restack
	if run == nil {
		run = s.App.Restack
	}
	res, err := run()
	if err == nil || !raced(err) {
		return res, err
	}
	_, _ = gitx.Run(s.App.Root, "fetch", "--quiet", "origin")
	return run()
}

// raced reports whether a restack failed only because another push moved a
// ref under it, which the next attempt fixes.
func raced(err error) bool {
	return errors.Is(err, app.ErrRemoteMoved) || strings.Contains(err.Error(), "stale info")
}

func (s *Sentinel) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// mayRestack reports whether the sentinel may restack for task now: fewer
// than maxRestacks tries, and the last one's backoff is over.
func (s *Sentinel) mayRestack(task string) bool {
	t := s.tries[task]
	return t.n < maxRestacks && !s.clock().Before(t.next)
}

// tried records a restack for task and backs the next one off, doubling from
// one Interval.
func (s *Sentinel) tried(task string) {
	if s.tries == nil {
		s.tries = map[string]restackTry{}
	}
	iv := s.Interval
	if iv <= 0 {
		iv = DefaultInterval
	}
	t := s.tries[task]
	t.n++
	t.next = s.clock().Add(iv << (t.n - 1))
	s.tries[task] = t
}

// restackFixes reports whether every part of a layer's problem is one a
// restack fixes: other tasks' commits or merges in its range, not descending
// from the layer below, missing landed commits, or landed work off
// integration. A lost range or a branch moved by hand is not; restack
// refuses those.
func restackFixes(problem string) bool {
	for _, p := range strings.Split(problem, "; ") {
		switch {
		case strings.HasPrefix(p, "does not descend from "),
			strings.HasPrefix(p, "contains "),
			strings.HasPrefix(p, "is missing "),
			strings.HasPrefix(p, "has ") && strings.Contains(p, ", landed "),
			strings.HasSuffix(p, "restack puts it back"):
		default:
			return false
		}
	}
	return true
}

// check is one locked pass over the stack. With restack set, it stops before
// flagging and reports fix when a restack may fix everything it found. A
// flag's cause names the sentinel's last failed restack for its task.
func (s *Sentinel) check(restack bool) (rep Report, fix bool, err error) {
	a := s.App
	unlock, ok, err := a.TryLockTrain()
	if err != nil {
		return Report{}, false, err
	}
	if !ok {
		return Report{Busy: true}, false, nil
	}
	defer unlock()

	ahead := s.baseAhead()
	info, err := a.ReconcileStack(s.GH.PR)
	if err != nil {
		return Report{}, false, err
	}
	layers, err := a.StackLayers()
	if err != nil {
		return Report{}, false, err
	}
	flag, flagged, err := a.Flag()
	if err != nil {
		return Report{}, false, err
	}
	inStack := map[string]bool{}
	for _, l := range layers {
		if l.Task.PR != "" {
			inStack[l.Task.PR] = true
		}
	}

	provider := config.ForgeGitHub
	if p, ok := s.GH.(interface{ Provider() string }); ok {
		provider = p.Provider()
	}
	fname, noun := forge.Name(provider), forge.PRNoun(provider)
	var hits []hit
	for i, l := range layers {
		if pr, ok := info[l.Task.PR]; ok && l.Task.PR != "" && pr.Mergeable == "CONFLICTING" {
			hits = append(hits, hit{i, i, fmt.Sprintf("%s reports its %s %s conflicts with its base", fname, noun, l.Task.PR), true, false})
		}
		if l.Problem != "" {
			hits = append(hits, hit{i, i, l.Problem, false, restackFixes(l.Problem)})
		}
	}
	if len(layers) > 0 && ahead > 0 {
		commits := "1 commit"
		if ahead > 1 {
			commits = fmt.Sprintf("%d commits", ahead)
		}
		hits = append(hits, hit{0, 0, fmt.Sprintf("origin/%s has %s %s lacks", a.Cfg.Base, commits, a.Cfg.Integration), false, true})
	}

	if len(hits) == 0 {
		s.tries = nil
		if flagged {
			return Report{}, false, s.clear(flag, inStack)
		}
		return Report{}, false, nil
	}

	first := hits[0]
	for _, h := range hits[1:] {
		if h.from < first.from {
			first = h
		}
	}
	rep = Report{AtRisk: true, Task: layers[first.from].Task.ID, Cause: first.cause}
	if first.from != first.at {
		rep.Cause = layers[first.at].Task.ID + ": " + first.cause
	}
	fix = true
	for _, h := range hits {
		rep.Hits = append(rep.Hits, layers[h.at].Task.ID+": "+h.cause)
		fix = fix && h.fix
	}
	if restack && fix && s.mayRestack(rep.Task) {
		return rep, true, nil
	}
	if t := s.tries[rep.Task]; t.err != "" {
		rep.Cause += "; the sentinel's restack failed: " + t.err
	}
	// needs-human goes on each conflicting PR and the PRs stacked on it in
	// its own PR stack, unless acked. Independent PRs later in train order
	// don't depend on the conflict and stay unlabeled (#845).
	rep.Acked = flagged && flag.Acked && flag.Task == rep.Task
	var want []string
	if !rep.Acked {
		for i, l := range layers {
			if l.Task.PR != "" && slices.ContainsFunc(hits, func(h hit) bool { return h.conflict && dependsOn(layers, i, h.from) }) {
				want = append(want, l.Task.PR)
			}
		}
	}

	next := app.StackFlag{Task: rep.Task, Cause: rep.Cause, Acked: rep.Acked}
	var errs []error
	for _, pr := range flag.PRs {
		if slices.Contains(want, pr) {
			next.PRs = append(next.PRs, pr)
			continue
		}
		if err := s.GH.RemoveLabel(pr, Label); err != nil {
			if inStack[pr] {
				errs = append(errs, err)
				next.PRs = append(next.PRs, pr) // retried next cycle
			}
			continue
		}
		if inStack[pr] {
			why := "the conflict that needed a human is gone; what is left, restack fixes without one"
			if rep.Acked {
				why = "the stack's flag was acknowledged (`saddle sentinel ack`)"
			}
			if err := s.comment(pr, liftComment(why)); err != nil {
				errs = append(errs, err)
			}
		}
	}
	for _, pr := range want {
		if slices.Contains(next.PRs, pr) {
			continue
		}
		if err := s.GH.AddLabel(pr, Label); err != nil {
			errs = append(errs, err)
			continue
		}
		next.PRs = append(next.PRs, pr)
	}
	rep.PRs = next.PRs
	for _, pr := range next.PRs {
		if i := slices.IndexFunc(layers, func(l app.StackLayer) bool { return l.Task.PR == pr }); i >= 0 {
			if err := s.comment(pr, flagComment(layers, hits, i, rep, a.Cfg.Base)); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if err := a.SetFlag(next); err != nil {
		return rep, false, errors.Join(append(errs, err)...)
	}
	if !rep.Acked && (!flagged || flag.Acked || flag.Task != next.Task || flag.Cause != next.Cause) {
		a.Store.Event(rep.Task, EventAtRisk, rep.Cause)
		errs = append(errs, a.Notify(app.OrchestratorID, store.NoticeAction, notice(rep, a.Cfg.Base)))
	}
	return rep, false, errors.Join(errs...)
}

func notice(rep Report, base string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Stack at risk from %s up: %s.", rep.Task, rep.Cause)
	if len(rep.Hits) > 1 {
		b.WriteString("\nEverything the sentinel found:")
		for _, h := range rep.Hits {
			b.WriteString("\n  " + h)
		}
	}
	if len(rep.PRs) > 0 {
		fmt.Fprintf(&b, "\nA conflict needs a human decision, so these PRs are labeled %s: %s.", Label, strings.Join(rep.PRs, ", "))
	}
	fmt.Fprintf(&b, "\nprs publishes only the layers below %s, and land holds queued work that touches the layers from there up, until the stack checks clean. "+
		"Run restack to rebuild it on origin/%s; don't fix it with git or a worker. The flag and labels clear by themselves once it is sound. "+
		"If restack can't fix it, `saddle unstack <task|pr>` drops a task from the stack and `saddle sentinel ack` acknowledges the flag; never edit state.db.", rep.Task, base)
	return b.String()
}

// clear lifts the flag and the labels it put on. A PR that has left the
// stack keeps whatever label removal GitHub allows; it never holds the flag.
func (s *Sentinel) clear(flag app.StackFlag, inStack map[string]bool) error {
	a := s.App
	var errs, left []string
	for _, pr := range flag.PRs {
		if err := s.GH.RemoveLabel(pr, Label); err != nil {
			if inStack[pr] {
				errs = append(errs, err.Error())
				left = append(left, pr)
			}
			continue
		}
		if !inStack[pr] {
			continue
		}
		if err := s.comment(pr, clearComment(flag)); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(left) > 0 {
		// Keep the flag so the next cycle retries the labels it couldn't remove.
		flag.PRs = left
		if err := a.SetFlag(flag); err != nil {
			errs = append(errs, err.Error())
		}
		return errors.New(strings.Join(errs, "; "))
	}
	if err := a.ClearFlag(); err != nil {
		return err
	}
	a.Store.Event(flag.Task, EventClear, flag.Cause)
	err := a.Notify(app.OrchestratorID, store.NoticeInfo,
		fmt.Sprintf("The PR stack checks clean again; the %s flag on %s is lifted and prs and land work again.", Label, flag.Task))
	if len(errs) > 0 {
		// Only resolution notes failed. The labels are off, so the flag goes anyway.
		err = errors.Join(errors.New(strings.Join(errs, "; ")), err)
	}
	return err
}

// baseAhead fetches origin/<base> and counts the commits it has that
// integration lacks. It is 0 when there is no origin or the fetch fails.
func (s *Sentinel) baseAhead() int {
	a := s.App
	remote := "refs/remotes/origin/" + a.Cfg.Base
	if _, err := gitx.Run(a.Root, "fetch", "--quiet", "origin", "+refs/heads/"+a.Cfg.Base+":"+remote); err != nil {
		return 0
	}
	n, err := gitx.CommitsBetween(a.Root, a.Cfg.Integration, remote)
	if err != nil {
		return 0
	}
	return n
}

// commentMarker tags the sentinel's comment on a PR, so it finds the comment
// again after a restart instead of posting another.
const commentMarker = "<!-- saddle-sentinel:needs-human -->"

// comment makes the sentinel's comment on pr read body: it posts one if the PR
// has none and edits it if it says something else.
func (s *Sentinel) comment(pr, body string) error {
	body = commentMarker + "\n" + body
	c, ok := s.posted[pr]
	if !ok {
		cs, err := s.GH.Comments(pr)
		if err != nil {
			return err
		}
		for _, x := range cs {
			if strings.Contains(x.Body, commentMarker) {
				c, ok = x, true
			}
		}
	}
	switch {
	case ok && c.Body == body:
	case ok:
		if err := s.GH.EditComment(c.ID, body); err != nil {
			return err
		}
		c.Body = body
	default:
		if err := s.GH.AddComment(pr, body); err != nil {
			return err
		}
		// Look the new comment up next time; gh pr comment doesn't print its ID.
		return nil
	}
	if s.posted == nil {
		s.posted = map[string]Comment{}
	}
	s.posted[pr] = c
	return nil
}

// flagComment explains why layers[i]'s PR is labeled: the conflicts at it
// and below it that put it at risk, and what else the sentinel found there.
func flagComment(layers []app.StackLayer, hits []hit, i int, rep Report, base string) string {
	var why []string
	for _, h := range hits {
		if h.at == i {
			why = append(why, fmt.Sprintf("This PR's branch `%s` (%s): %s.", layers[i].Task.Branch, layers[i].Task.ID, h.cause))
		}
	}
	for _, h := range hits {
		if h.conflict && h.at < i && dependsOn(layers, i, h.from) && dependsOn(layers, i, h.at) {
			below := layers[h.at].Task
			why = append(why, fmt.Sprintf("It is stacked on %s (`%s`, %s), where: %s.", below.ID, below.Branch, below.PR, h.cause))
		}
	}
	if len(why) == 0 {
		// Labeled on an earlier tick, below what is wrong now.
		why = append(why, fmt.Sprintf("Saddle flagged the stack from %s up: %s.", rep.Task, rep.Cause))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**Saddle labeled this PR `%s`.** Don't merge it until the label is gone.\n\n**Why:** a conflict needs someone to decide how to resolve it.\n", Label)
	for _, w := range why {
		b.WriteString("- " + w + "\n")
	}
	fmt.Fprintf(&b, "\n**What's needed:** run `saddle restack` (or have the orchestrator run it) to rebuild the stack on origin/%s. "+
		"If a commit still conflicts, restack moves nothing and hands the conflict to the task that owns it; a human has to decide how to resolve it. "+
		"Don't fix the branch by hand with git or a worker. If the task's work should not ship after all, `saddle unstack <task|pr>` drops it from the stack; "+
		"`saddle sentinel ack` acknowledges the flag and takes this label off.\n\n"+
		"**What clears it:** nothing to do on this PR. Once the conflict is gone, the sentinel removes the label and updates this comment.\n", base)
	return b.String()
}

// dependsOn reports whether layer i is layer at or stacked on it.
func dependsOn(layers []app.StackLayer, i, at int) bool {
	return i == at || app.StackedOn(layers, i, at)
}

// clearComment replaces a flag comment once the label is lifted.
func clearComment(flag app.StackFlag) string {
	return fmt.Sprintf("**Cleared:** the PR stack checks clean again, so Saddle removed the `%s` label. "+
		"It had been flagged from %s up: %s.\n", Label, flag.Task, flag.Cause)
}

// liftComment replaces a flag comment when the label comes off while the
// stack is still flagged.
func liftComment(why string) string {
	return fmt.Sprintf("**Label removed:** Saddle took the `%s` label off this PR: %s.\n", Label, why)
}
