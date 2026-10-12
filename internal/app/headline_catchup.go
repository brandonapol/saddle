package app

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/automerge"
	"github.com/brandonapol/saddle/internal/store"
)

// Catch-up (#403) answers "what happened since I last looked" in a few
// plain bullets, built from the event log by Go, not by a model: what's
// waiting on the owner first, then what's stuck, what got done, what
// started, and where things stand now.

// maxCatchUp caps the bullets.
const maxCatchUp = 5

// CatchUpDefault is how far back a catch-up looks when nobody has looked.
var CatchUpDefault = 12 * time.Hour

// catchUpEvents caps the events a catch-up reads.
const catchUpEvents = 20000

// CatchUp turns the events since the last look into at most five bullets.
// tasks is the current task list: an ask already answered, or trouble on a
// task that has since landed, is left out. now is the current headline.
func CatchUp(es []store.Event, tasks []store.Task, now StateHeadline) []string {
	byID := map[string]store.Task{}
	for _, t := range tasks {
		byID[t.ID] = t
	}
	label := func(id string) string {
		if t, ok := byID[id]; ok {
			return taskLabel(t)
		}
		return id
	}
	var (
		asks, stuck, landed, started []string
		askText                      = map[string]string{}
		stuckSeen                    = map[string]bool{}
		landedSeen                   = map[string]bool{}
		startedSeen                  = map[string]bool{}
		merged                       int
	)
	for _, e := range es {
		t, known := byID[e.Task]
		switch e.Kind {
		case "notification":
			if known && t.Status != store.NeedsYou {
				continue
			}
			if _, seen := askText[e.Task]; !seen {
				asks = append(asks, e.Task)
			}
			askText[e.Task] = e.Data
		case "landed":
			if !landedSeen[e.Task] {
				landedSeen[e.Task] = true
				landed = append(landed, e.Task)
			}
		case automerge.EventMerged:
			merged++
		case "spawn":
			if e.Task != "" && !startedSeen[e.Task] {
				startedSeen[e.Task] = true
				started = append(started, e.Task)
			}
		default:
			what := stuckWhat(e)
			if what == "" || stuckSeen[e.Task+what] || (known && t.Status == store.Landed) {
				continue
			}
			stuckSeen[e.Task+what] = true
			who := label(e.Task)
			if e.Task == "" {
				who = "saddle"
			}
			stuck = append(stuck, who+" "+what)
		}
	}
	var out []string
	switch len(asks) {
	case 0:
	case 1:
		what := "is waiting at a prompt"
		if s := strings.TrimSpace(askText[asks[0]]); s != "" {
			what = "asks: " + clipText(s, maxAsk)
		}
		out = append(out, fmt.Sprintf("Waiting on you: %s %s", label(asks[0]), what))
	default:
		ls := make([]string, len(asks))
		for i, id := range asks {
			ls[i] = label(id)
		}
		out = append(out, fmt.Sprintf("Waiting on you: %d agents (%s).", len(asks), joinSome(ls, 3)))
	}
	if len(stuck) > 0 {
		out = append(out, "Stuck: "+joinReasons(stuck)+".")
	}
	if len(landed) > 0 || merged > 0 {
		var parts []string
		if len(landed) > 0 {
			ls := make([]string, len(landed))
			for i, id := range landed {
				ls[i] = label(id)
			}
			parts = append(parts, fmt.Sprintf("%d landed (%s)", len(landed), joinSome(ls, 3)))
		}
		if merged > 0 {
			parts = append(parts, countOf(merged, "PR")+" merged")
		}
		out = append(out, "Done: "+strings.Join(parts, "; ")+".")
	}
	if len(started) > 0 {
		ls := make([]string, len(started))
		for i, id := range started {
			ls[i] = label(id)
		}
		out = append(out, "Started: "+joinSome(ls, 4)+".")
	}
	if len(out) == 0 {
		out = append(out, "Nothing new since you last looked.")
	}
	if now.Text != "" {
		out = append(out, "Now: "+now.Text)
	}
	if len(out) > maxCatchUp {
		out = out[:maxCatchUp]
	}
	return out
}

// stuckWhat says how an event left its task stuck, or "" when it didn't.
func stuckWhat(e store.Event) string {
	detail := func(s string) string {
		if d := clipText(e.Data, 40); d != "" {
			return s + " (" + d + ")"
		}
		return s
	}
	switch e.Kind {
	case "train_conflict", "restack_conflict":
		return "hit a conflict"
	case "train_test_failed":
		return "failed the merge train's tests"
	case "ci_red":
		return detail("CI went red")
	case automerge.EventFailed:
		return detail("auto-merge failed")
	case "stack_at_risk":
		return "put the stack at risk"
	case EventAdapterExhausted:
		return detail("ran out of quota")
	}
	return ""
}

// CatchUp reads the events after since and returns the catch-up bullets.
func (a *App) CatchUp(since, now time.Time) ([]string, error) {
	all, err := a.Store.Events(catchUpEvents)
	if err != nil {
		return nil, err
	}
	var es []store.Event
	for _, e := range all {
		if e.TS.After(since) {
			es = append(es, e)
		}
	}
	tasks, err := a.Store.Tasks()
	if err != nil {
		return nil, err
	}
	h, err := a.Headline(now)
	if err != nil {
		return nil, err
	}
	return CatchUp(es, tasks, h), nil
}

func (a *App) lookedPath() string { return a.stateDir("last-looked") }

// MarkLooked records when the owner last looked: their last keystroke in
// the TUI. A catch-up without --since starts there.
func (a *App) MarkLooked(t time.Time) error {
	return os.WriteFile(a.lookedPath(), []byte(strconv.FormatInt(t.Unix(), 10)+"\n"), 0o644)
}

// LastLooked is when the owner last looked, or zero when unknown.
func (a *App) LastLooked() time.Time {
	b, err := os.ReadFile(a.lookedPath())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			a.Store.Event("", "catchup_error", err.Error())
		}
		return time.Time{}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

// CatchUpSince is where a catch-up starts by default: the last look, else
// CatchUpDefault ago.
func (a *App) CatchUpSince(now time.Time) time.Time {
	if t := a.LastLooked(); !t.IsZero() {
		return t
	}
	return now.Add(-CatchUpDefault)
}
