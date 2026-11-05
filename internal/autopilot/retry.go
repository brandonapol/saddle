package autopilot

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Retries (#767). A task that was killed, failed to start or ended with no
// change frees its issue: autopilot respawns it, up to MaxRetries times per
// run, each after RetryBackoff and with what the last attempt said in its
// brief. After that the issue is labelled needs-human with the reasons, and
// stops counting as ready; removing the label gives it a fresh budget.

// Ways a task ends without landing, in Task.End.
const (
	EndKilled   = "killed"
	EndFailed   = "failed"
	EndNoChange = "no_change"
)

const (
	// MaxRetries is how many times one run respawns a freed issue.
	MaxRetries = 2
	// DefaultRetryBackoff is how long a freed issue waits before its retry.
	DefaultRetryBackoff = 5 * time.Minute
	// NeedsHumanLabel marks an issue autopilot gave up on.
	NeedsHumanLabel = "needs-human"
)

// EventGiveUp is logged when an issue runs out of retries.
const EventGiveUp = "autopilot_give_up"

// EventClose is logged when a no-change task's issue is closed as already done.
const EventClose = "autopilot_close"

// Tracker is an Env that can act on issues: close one, give up on one
// (label it needs-human and comment), and say whether one is closed.
type Tracker interface {
	CloseIssue(number int, comment string) error
	GiveUp(number int, comment string) error
	IssueClosed(number int) (bool, error)
}

// alreadyDone matches a no-change summary that says the work was done before.
var alreadyDone = regexp.MustCompile(`(?i)\b(already\s+(been\s+)?(done|fixed|implemented|landed|merged|resolved|handled|covered|present|in\s+place|exists?|on\s+(main|master|integration))` +
	`|(exists?|in\s+place|there|present)\s+already` +
	`|nothing\s+(left\s+)?to\s+(do|change|fix|implement)` +
	`|no\s+changes?\s+(were\s+|was\s+|is\s+|are\s+)?(needed|required|necessary))\b`)

// AlreadyDone reports whether a no-change task's summary says the issue's
// work was already done, so the issue can be closed rather than retried.
func AlreadyDone(summary string) bool { return alreadyDone.MatchString(summary) }

// ended is a task that ended without landing, described for a brief.
func (t Task) ended() string {
	s := t.ID + " "
	switch t.End {
	case EndKilled:
		s += "was killed"
	case EndFailed:
		s += "failed to start"
	case EndNoChange:
		s += "ended with no change"
	default:
		s += "ended"
	}
	if w := strings.TrimSpace(t.Why); w != "" {
		s += " (" + oneLine(w) + ")"
	}
	if sum := strings.TrimSpace(t.Summary); sum != "" {
		s += "; its last summary: " + oneLine(sum)
	}
	return s
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// ledger is who holds each issue, built from the tasks and this run's spawns.
type ledger struct {
	taken map[int]Task   // a live, queued, landed or otherwise unfinished task
	dead  map[int][]Task // tasks that ended without landing, oldest first
	tries map[int]int    // this run's spawns for the issue
	ended map[int]time.Time
}

// newLedger sorts tasks by issue. A Spawned task missing from tasks was
// killed and its record pruned. It stamps Ended on spawns seen dead for
// the first time.
func newLedger(st *State, tasks []Task, now time.Time) ledger {
	l := ledger{taken: map[int]Task{}, dead: map[int][]Task{}, tries: map[int]int{}, ended: map[int]time.Time{}}
	add := func(issue int, t Task) {
		if t.End == "" {
			if _, ok := l.taken[issue]; !ok || t.Live {
				l.taken[issue] = t
			}
			return
		}
		if !slices.ContainsFunc(l.dead[issue], func(d Task) bool { return d.ID == t.ID }) {
			l.dead[issue] = append(l.dead[issue], t)
		}
	}
	for _, t := range tasks {
		if t.Issue > 0 {
			add(t.Issue, t)
		}
	}
	for i := range st.Spawned {
		s := &st.Spawned[i]
		l.tries[s.Issue]++
		j := slices.IndexFunc(tasks, func(t Task) bool { return t.ID == s.Task })
		t := Task{ID: s.Task, Issue: s.Issue, End: EndKilled, Why: "its task record is gone"}
		if j >= 0 {
			t = tasks[j]
		}
		if t.End != "" && s.Ended.IsZero() {
			s.Ended, s.Why = now, t.ended()
		}
		if !s.Ended.IsZero() {
			l.ended[s.Issue] = s.Ended
		}
		add(s.Issue, t)
	}
	return l
}

// retry decides what a freed issue does: "" and the last attempt when it may
// spawn, a wait reason while the backoff runs, or done when it was handled
// (closed or given up) and isn't ready.
type retryVerdict struct {
	why     string // the backoff it waits on
	prev    *Task  // the last attempt, for the brief
	handled string // closed or given up: not ready
}

func (d *Driver) retry(st *State, l ledger, is Issue, now time.Time) retryVerdict {
	ds := l.dead[is.Number]
	if len(ds) == 0 {
		return retryVerdict{}
	}
	last := ds[len(ds)-1]
	if last.End == EndNoChange && AlreadyDone(last.Summary) {
		if !slices.Contains(st.Closed, is.Number) {
			st.Closed = append(st.Closed, is.Number)
			msg := fmt.Sprintf("Saddle autopilot closed this: %s's agent found nothing to change.\n\n%s", last.ID, strings.TrimSpace(last.Summary))
			if tr, ok := d.Env.(Tracker); ok {
				if err := tr.CloseIssue(is.Number, msg); err != nil {
					logErr(d.Env, fmt.Sprintf("close #%d", is.Number), err)
				}
			}
			d.Env.Event(EventClose, fmt.Sprintf("#%d: %s", is.Number, oneLine(last.Summary)))
		}
		return retryVerdict{handled: "closed: " + last.ID + " found it already done"}
	}
	if l.tries[is.Number] > MaxRetries {
		d.giveUp(st, is, ds)
		return retryVerdict{handled: fmt.Sprintf("labelled %s after %d attempts", NeedsHumanLabel, l.tries[is.Number])}
	}
	if at := l.ended[is.Number]; !at.IsZero() {
		if next := at.Add(orDefault(d.RetryBackoff, DefaultRetryBackoff)); now.Before(next) {
			return retryVerdict{why: fmt.Sprintf("retry %d of %d at %s: %s", l.tries[is.Number], MaxRetries, clock(next), last.ended())}
		}
	}
	return retryVerdict{prev: &last}
}

// giveUp labels is needs-human and comments the attempts' reasons, once.
func (d *Driver) giveUp(st *State, is Issue, ds []Task) {
	if slices.Contains(st.GaveUp, is.Number) {
		return
	}
	st.GaveUp = append(st.GaveUp, is.Number)
	var b strings.Builder
	fmt.Fprintf(&b, "Saddle autopilot gave up on this after %d attempts that didn't land:\n", len(ds))
	for _, t := range ds {
		b.WriteString("\n- " + t.ended())
	}
	fmt.Fprintf(&b, "\n\nRemove the %s label to let autopilot try again.", NeedsHumanLabel)
	if tr, ok := d.Env.(Tracker); ok {
		if err := tr.GiveUp(is.Number, b.String()); err != nil {
			logErr(d.Env, fmt.Sprintf("give up on #%d", is.Number), err)
		}
	}
	d.Env.Event(EventGiveUp, fmt.Sprintf("#%d after %d attempts", is.Number, len(ds)))
	d.Env.Notify(false, fmt.Sprintf("Autopilot gave up on #%d %q after %d attempts and labelled it %s.", is.Number, is.Title, len(ds), NeedsHumanLabel))
}

// hasLabel reports whether is carries label, case-insensitively.
func (is Issue) hasLabel(label string) bool {
	return slices.ContainsFunc(is.Labels, func(l string) bool { return strings.EqualFold(l, label) })
}

// retryBrief is what a respawn's brief adds about the last attempt.
func retryBrief(prev *Task, attempt int) string {
	if prev == nil {
		return ""
	}
	return fmt.Sprintf("## Previous attempt\nThis is attempt %d. The last one didn't land: %s. Learn from it; "+
		"if its branch has useful commits, build on them.\n\n", attempt, prev.ended())
}
