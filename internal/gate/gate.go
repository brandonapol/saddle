// Package gate holds a task back until a wait-until condition is met: another
// task lands or finishes, a PR merges, a claim frees, a commit touches some
// paths, or a time passes.
//
// Conditions are pure. They read the world only through the small interfaces
// bundled in State and the clock passed to Eval, so callers can evaluate them
// against the store, git and GitHub, and tests can evaluate them against fakes.
// Evaluation is deterministic: the same inputs give the same Result, reason
// text included.
package gate

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/claims"
)

// TaskInfo is what conditions need to know about a task.
type TaskInfo struct {
	Status string // free-form, shown in reasons ("running", "queued", ...)
	Done   bool   // the agent called done
	Landed bool   // the merge train landed its branch
}

// Commit is one commit on a task's branch.
type Commit struct {
	SHA   string
	At    time.Time
	Paths []string // repo-relative paths it touched
}

// Tasks looks up a task by id.
type Tasks interface {
	Task(id string) (TaskInfo, bool)
}

// PRs reports whether a pull request merged. ok is false for an unknown PR.
type PRs interface {
	PRMerged(number int) (merged, ok bool)
}

// Claims returns every task's claim globs, keyed by task id.
type Claims interface {
	Claims() map[string][]string
}

// Commits lists a task's commits.
type Commits interface {
	Commits(task string) []Commit
}

// State is everything a condition may read. A nil source leaves the
// conditions that need it blocked, never ready.
type State struct {
	Tasks   Tasks
	PRs     PRs
	Claims  Claims
	Commits Commits
}

// Result is the outcome of evaluating a condition. Reason says why it is
// ready, or what it is still waiting on.
type Result struct {
	Ready  bool
	Reason string
}

// Cond is a wait-until condition.
type Cond interface {
	Eval(now time.Time, s State) Result
	// String is a compact description, e.g. "any(landed(t4), after(12:00:00 UTC))".
	String() string
	// Deps lists the task ids the condition waits on, sorted, for deadlock
	// detection.
	Deps() []string
}

func ready(format string, a ...any) Result   { return Result{true, fmt.Sprintf(format, a...)} }
func blocked(format string, a ...any) Result { return Result{false, fmt.Sprintf(format, a...)} }

func clock(t time.Time) string { return t.Format("15:04:05 MST") }

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// TaskLanded waits until task's branch lands.
func TaskLanded(task string) Cond { return landed{task} }

type landed struct{ task string }

func (c landed) String() string { return "landed(" + c.task + ")" }
func (c landed) Deps() []string { return []string{c.task} }
func (c landed) Eval(_ time.Time, s State) Result {
	wait := "waiting on " + c.task + " to land"
	if s.Tasks == nil {
		return blocked("%s (no task state)", wait)
	}
	ti, ok := s.Tasks.Task(c.task)
	switch {
	case !ok:
		return blocked("%s (unknown task)", wait)
	case ti.Landed:
		return ready("%s landed", c.task)
	}
	return blocked("%s (%s)", wait, ti.Status)
}

// TaskDone waits until task finishes: it called done, or already landed.
func TaskDone(task string) Cond { return done{task} }

type done struct{ task string }

func (c done) String() string { return "done(" + c.task + ")" }
func (c done) Deps() []string { return []string{c.task} }
func (c done) Eval(_ time.Time, s State) Result {
	wait := "waiting on " + c.task + " to finish"
	if s.Tasks == nil {
		return blocked("%s (no task state)", wait)
	}
	ti, ok := s.Tasks.Task(c.task)
	switch {
	case !ok:
		return blocked("%s (unknown task)", wait)
	case ti.Done || ti.Landed:
		return ready("%s is done", c.task)
	}
	return blocked("%s (%s)", wait, ti.Status)
}

// PRMerged waits until pull request number merges.
func PRMerged(number int) Cond { return merged{number} }

type merged struct{ number int }

func (c merged) String() string { return fmt.Sprintf("merged(#%d)", c.number) }
func (c merged) Deps() []string { return nil }
func (c merged) Eval(_ time.Time, s State) Result {
	wait := fmt.Sprintf("waiting on PR #%d to merge", c.number)
	if s.PRs == nil {
		return blocked("%s (no PR state)", wait)
	}
	m, ok := s.PRs.PRMerged(c.number)
	switch {
	case !ok:
		return blocked("%s (unknown PR)", wait)
	case m:
		return ready("PR #%d merged", c.number)
	}
	return blocked("%s", wait)
}

// ClaimFree waits until no task other than except holds a claim that could
// cover path. path may itself be a glob.
func ClaimFree(path, except string) Cond { return free{path, except} }

type free struct{ path, except string }

func (c free) String() string {
	if c.except == "" {
		return "free(" + c.path + ")"
	}
	return "free(" + c.path + " except " + c.except + ")"
}
func (c free) Deps() []string { return nil }
func (c free) Eval(_ time.Time, s State) Result {
	if s.Claims == nil {
		return blocked("waiting on %s to be free (no claim state)", c.path)
	}
	all := s.Claims.Claims()
	tasks := make([]string, 0, len(all))
	for t := range all {
		tasks = append(tasks, t)
	}
	slices.Sort(tasks) // name the same owner every time
	for _, t := range tasks {
		if t == c.except {
			continue
		}
		for _, g := range all[t] {
			if claims.Overlap(g, c.path) {
				return blocked("waiting on %s to release %s (claimed by %s as %s)", t, c.path, t, g)
			}
		}
	}
	return ready("%s is free", c.path)
}

// After waits until the clock reaches at. Use it for timeouts:
// Any(TaskLanded("t4"), After(created.Add(30*time.Minute))).
func After(at time.Time) Cond { return after{at} }

type after struct{ at time.Time }

func (c after) String() string { return "after(" + clock(c.at) + ")" }
func (c after) Deps() []string { return nil }
func (c after) Eval(now time.Time, _ State) Result {
	if now.Before(c.at) {
		return blocked("waiting until %s (%s left)", clock(c.at), c.at.Sub(now).Round(time.Second))
	}
	return ready("%s passed", clock(c.at))
}

// CommitTouching waits until task commits, at or after since, a change to a
// path matching one of paths (claim globs). With no paths any commit counts.
func CommitTouching(task string, since time.Time, paths ...string) Cond {
	return commit{task, since, paths}
}

type commit struct {
	task  string
	since time.Time
	paths []string
}

func (c commit) String() string {
	if len(c.paths) == 0 {
		return "commit(" + c.task + ")"
	}
	return "commit(" + c.task + ", touching=" + strings.Join(c.paths, ", ") + ")"
}
func (c commit) Deps() []string { return []string{c.task} }
func (c commit) Eval(_ time.Time, s State) Result {
	wait := "waiting on " + c.task + "'s next commit"
	if len(c.paths) > 0 {
		wait = "waiting on " + c.task + "'s commit to " + strings.Join(c.paths, ", ")
	}
	if s.Commits == nil {
		return blocked("%s (no commit state)", wait)
	}
	for _, cm := range s.Commits.Commits(c.task) {
		if cm.At.Before(c.since) {
			continue
		}
		if len(c.paths) == 0 {
			return ready("%s committed %s", c.task, short(cm.SHA))
		}
		for _, p := range cm.Paths {
			for _, g := range c.paths {
				if claims.Match(g, p) {
					return ready("%s committed %s to %s", c.task, short(cm.SHA), p)
				}
			}
		}
	}
	return blocked("%s", wait)
}

// All is ready once every condition is. Its reason lists every unmet
// condition in order, or every met one when ready. All() is ready.
func All(cs ...Cond) Cond { return all(cs) }

type all []Cond

func (c all) String() string { return "all(" + join(c) + ")" }
func (c all) Deps() []string { return deps(c) }
func (c all) Eval(now time.Time, s State) Result {
	if len(c) == 0 {
		return ready("no conditions")
	}
	var met, unmet []string
	for _, x := range c {
		r := x.Eval(now, s)
		if r.Ready {
			met = append(met, r.Reason)
		} else {
			unmet = append(unmet, r.Reason)
		}
	}
	if len(unmet) > 0 {
		return Result{false, strings.Join(unmet, "; ")}
	}
	return Result{true, strings.Join(met, "; ")}
}

// Any is ready once one condition is, with that condition's reason. Any()
// is never ready.
func Any(cs ...Cond) Cond { return anyOf(cs) }

type anyOf []Cond

func (c anyOf) String() string { return "any(" + join(c) + ")" }
func (c anyOf) Deps() []string { return deps(c) }
func (c anyOf) Eval(now time.Time, s State) Result {
	if len(c) == 0 {
		return blocked("no conditions")
	}
	var unmet []string
	for _, x := range c {
		r := x.Eval(now, s)
		if r.Ready {
			return r
		}
		unmet = append(unmet, r.Reason)
	}
	return Result{false, strings.Join(unmet, ", or ")}
}

func join(cs []Cond) string {
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = c.String()
	}
	return strings.Join(parts, ", ")
}

func deps(cs []Cond) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Deps()...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}
