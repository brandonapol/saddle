package app

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// Landing ends with an open PR against base (#819). The train still lands
// work on its own local branch first, so each queued branch is tested on top
// of everything landed before it, but that branch is an internal detail: no
// work counts as finished until its PR merges into base. These are the words
// status, notices and briefs use for each step of that, so a landed task is
// never called done or merged while it is only on the local train.
const (
	// DeliveryNoPR is a landed task whose PR isn't open yet. Saddle opens it
	// itself (land, the sentinel, and the first run after an upgrade).
	DeliveryNoPR = "landed locally, no PR yet"
	// DeliveryPROpen is a landed task whose PR is open but not merged.
	DeliveryPROpen = "landed locally, PR open"
	// DeliveryMerged is a task whose work is on base: the only finish line.
	DeliveryMerged = "merged"
)

// EventLandedNoPR is the alarm for a landed task with no PR. It is raised
// once per task, and cleared by the task's PR opening.
const EventLandedNoPR = "landed_no_pr"

// EventPROpened records the PR saddle opened for a task right after it landed.
const EventPROpened = "pr_opened"

// Delivery says how far a task's work has got toward base, in the words
// above: "" for a task that hasn't landed. trainState is its train entry's
// state, and pr its PR.
func Delivery(status, trainState, pr string) string {
	switch {
	case trainState == TrainMerged:
		return DeliveryMerged
	case trainState == TrainSuperseded:
		return TrainSuperseded
	case status != store.Landed && trainState != store.TrainOK:
		return ""
	case pr == "":
		return DeliveryNoPR
	default:
		return DeliveryPROpen
	}
}

// Unpublished lists, in train order, the tasks in the PR stack that have no
// PR: their work is only on the local train and can't reach base.
func (a *App) Unpublished() ([]string, error) {
	stack, err := a.landedStack()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range stack {
		if l.PR == "" && l.Lost == "" {
			out = append(out, l.ID)
		}
	}
	return out, nil
}

// PublishBySentinel is PublishLanded's why for the sentinel's cycle, whose
// retries after a failure wait publishRetry.
const PublishBySentinel = "sentinel"

// publishRetry is how long the sentinel waits to retry a publish that left
// the same tasks without PRs: a red pre-publish gate isn't cached, and
// rerunning the repo's checks every two-minute cycle would load the machine.
const publishRetry = 15 * time.Minute

// failedPublishes maps a repo root to its last publish that left tasks
// without PRs: which tasks, and when.
var failedPublishes sync.Map

type failedPublish struct {
	ids string
	at  time.Time
}

// PublishLanded opens the PRs of every landed task that has none, the way
// prs does, so nobody has to remember to run it (#819). Land calls it right
// after the train, the sentinel on every cycle, and so the first run after an
// upgrade opens the PRs of tasks an older saddle landed without one. The
// sentinel's retries of a publish that left the same tasks without PRs wait
// publishRetry; land always tries. It
// returns the tasks it opened PRs for. A repo with no remote has nowhere to
// push, so nothing happens. Tasks still without a PR afterwards raise the
// landed-with-no-PR alarm once each: an event, and a notice to the
// orchestrator saying why.
func (a *App) PublishLanded(why string) ([]string, error) {
	if a.remote() == "" {
		return nil, nil
	}
	before, err := a.Unpublished()
	if err != nil || len(before) == 0 {
		failedPublishes.Delete(a.Root)
		return nil, err
	}
	key := a.publishKey(before)
	if why == PublishBySentinel {
		if f, ok := failedPublishes.Load(a.Root); ok && f.(failedPublish).ids == key && time.Since(f.(failedPublish).at) < publishRetry {
			return nil, nil // the same tasks failed a moment ago; land or a later cycle retries
		}
	}
	_, perr := a.PRs()
	after, err := a.Unpublished()
	if err != nil {
		return nil, err
	}
	if len(after) > 0 {
		failedPublishes.Store(a.Root, failedPublish{ids: a.publishKey(after), at: time.Now()})
	} else {
		failedPublishes.Delete(a.Root)
	}
	var opened []string
	for _, id := range before {
		if slices.Contains(after, id) {
			continue
		}
		opened = append(opened, id)
		if t, err := a.Store.Task(id); err == nil {
			a.Store.Event(id, EventPROpened, why+": "+t.PR)
		}
	}
	a.alarmUnpublished(after, perr)
	return opened, perr
}

// publishKey names a publish attempt: the tasks without PRs, and the stack
// flag, whose clearing makes a retry worth it at once.
func (a *App) publishKey(ids []string) string {
	key := strings.Join(ids, ",")
	if f, ok, _ := a.Flag(); ok {
		key += fmt.Sprintf(" flag %s %t %s", f.Task, f.Acked, f.Cause)
	}
	return key
}

// alarmUnpublished raises the landed-with-no-PR alarm for each of ids that
// hasn't had one since it last landed.
func (a *App) alarmUnpublished(ids []string, cause error) {
	if len(ids) == 0 {
		return
	}
	evs, _ := a.Store.Events(-1)
	raised := map[string]bool{}
	for _, e := range evs {
		switch e.Kind {
		case EventLandedNoPR:
			raised[e.Task] = true
		case "landed":
			raised[e.Task] = false
		}
	}
	var fresh []string
	for _, id := range ids {
		if !raised[id] {
			fresh = append(fresh, id)
		}
	}
	if len(fresh) == 0 {
		return
	}
	reason := "prs left them for now (a red pre-publish gate, red CI below them, a flagged stack, or a dependency whose PR hasn't merged)"
	if cause != nil {
		reason = "opening the PRs failed: " + firstLineOf(cause.Error())
	}
	for _, id := range fresh {
		a.Store.Event(id, EventLandedNoPR, reason)
	}
	_ = a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
		"%s landed locally but %s no PR, so %s not on %s and not done: %s. Saddle retries on every sentinel cycle and land; "+
			"status shows each as %q until its PR opens. Fix the cause, or run prs to see it.",
		strings.Join(fresh, ", "), hasHave(len(fresh)), itsTheir(len(fresh)), a.Cfg.Base, reason, DeliveryNoPR))
}

func hasHave(n int) string {
	if n == 1 {
		return "has"
	}
	return "have"
}

func itsTheir(n int) string {
	if n == 1 {
		return "its work is"
	}
	return "their work is"
}

func firstLineOf(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}
