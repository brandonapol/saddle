package app

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/runq"
)

// EventRunqBackpressure is logged when spawn refuses because the heavy-run
// queue is backed up (#243). The narrator mentions it (#242).
const EventRunqBackpressure = "runq_backpressure"

// HeavyRuns is the machine's heavy-run queue as saddle status, the MCP
// status tool, the TUI and the narrator show it (#242, docs/runq.md Q5).
type HeavyRuns struct {
	Mode    string       `json:"mode"`
	Repo    string       `json:"repo,omitempty"` // this repo; entries from others name theirs
	Classes []HeavyClass `json:"classes"`
}

// HeavyClass is one class's slots, holders and waiters in grant order.
type HeavyClass struct {
	Class    string       `json:"class"`
	Slots    int          `json:"slots"`
	Drained  bool         `json:"drained,omitempty"`
	MaxRunMS int64        `json:"max_run_ms"`
	Holders  []HeavyEntry `json:"holders"`
	Waiters  []HeavyEntry `json:"waiters"`
}

// HeavyEntry is one lease. Task and Repo come from its label and the repo
// that asked; a run outside any task has no Task ("pid N").
type HeavyEntry struct {
	Lease    string `json:"lease"`
	Task     string `json:"task,omitempty"`
	Repo     string `json:"repo,omitempty"`
	Label    string `json:"label"`
	Cmd      string `json:"cmd"`
	PID      int    `json:"pid"`
	Position int    `json:"position,omitempty"` // waiters: 1 is next
	AgeMS    int64  `json:"age_ms"`             // holders: running for; waiters: waiting for
	ETAMS    int64  `json:"eta_ms,omitempty"`   // waiters: median run × turns ahead; 0 without history
	Overdue  bool   `json:"overdue,omitempty"`  // holders past the class's max_run
}

// Age is how long the holder has run or the waiter has waited.
func (e HeavyEntry) Age() time.Duration { return time.Duration(e.AgeMS) * time.Millisecond }

// ETA is the waiter's estimated wait, 0 when unknown.
func (e HeavyEntry) ETA() time.Duration { return time.Duration(e.ETAMS) * time.Millisecond }

// Who names the entry for a reader in repo: the task alone in the same
// repo, "repo/task" from another, else the raw label.
func (e HeavyEntry) Who(repo string) string {
	switch {
	case e.Task == "":
		return e.Label
	case e.Repo == "" || e.Repo == repo:
		return e.Task
	}
	return e.Repo + "/" + e.Task
}

// Busy reports whether any class has a holder or a waiter.
func (v HeavyRuns) Busy() bool {
	for _, c := range v.Classes {
		if c.Busy() {
			return true
		}
	}
	return false
}

// Busy reports whether the class has a holder or a waiter.
func (c HeavyClass) Busy() bool { return len(c.Holders)+len(c.Waiters) > 0 }

// MaxRun is the class's max_run.
func (c HeavyClass) MaxRun() time.Duration { return time.Duration(c.MaxRunMS) * time.Millisecond }

// Segment is the class in a few words for a status bar:
// "go-test ▸t83 3m · 2 waiting".
func (c HeavyClass) Segment(repo string) string {
	s := c.Class
	for _, h := range c.Holders {
		s += " ▸" + h.Who(repo) + " " + RoughDuration(h.Age())
		if h.Overdue {
			s += "!"
		}
	}
	if c.Drained {
		s += " drained"
	}
	if n := len(c.Waiters); n > 0 {
		s += fmt.Sprintf(" · %d waiting", n)
	}
	return s
}

// RoughDuration rounds d for humans: seconds under a minute, minutes under
// an hour, else hours and minutes.
func RoughDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(time.Second).String()
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Round(time.Minute)/time.Minute))
	}
	d = d.Round(time.Minute)
	return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
}

// taskOf maps a lease label to the saddle task that asked: SADDLE_TASK, or
// "" for a run outside any task ("pid N").
func taskOf(label string) string {
	if label == "" || strings.HasPrefix(label, "pid ") {
		return ""
	}
	return label
}

// repoName is how leases name the repo at root.
func repoName(root string) string {
	if root == "" {
		return ""
	}
	return filepath.Base(root)
}

// HeavyRuns reads the machine's heavy-run queue.
func (a *App) HeavyRuns() (HeavyRuns, error) { return a.Heavy().Runs() }

// Runs reads the queue: it reaps dead leases first, like saddle runq status.
func (h Heavy) Runs() (HeavyRuns, error) {
	q, cfg, err := h.Open()
	if err != nil {
		return HeavyRuns{}, err
	}
	defer func() { _ = q.Close() }()
	st, err := q.Status()
	if err != nil {
		return HeavyRuns{}, err
	}
	v := HeavyRuns{Mode: string(q.Mode()), Repo: repoName(h.Root), Classes: []HeavyClass{}}
	for _, cs := range st {
		maxRun := cfg.MaxRunFor(cs.Class)
		c := HeavyClass{Class: cs.Class, Slots: cs.Slots, Drained: cs.Slots == 0, MaxRunMS: maxRun.Milliseconds(),
			Holders: []HeavyEntry{}, Waiters: []HeavyEntry{}}
		for _, e := range cs.Holders {
			he := heavyEntry(e)
			he.Overdue = e.Age > maxRun
			c.Holders = append(c.Holders, he)
		}
		for _, e := range cs.Waiters {
			c.Waiters = append(c.Waiters, heavyEntry(e))
		}
		v.Classes = append(v.Classes, c)
	}
	return v, nil
}

func heavyEntry(e runq.Entry) HeavyEntry {
	return HeavyEntry{Lease: e.Token, Task: taskOf(e.Label), Repo: e.Repo, Label: e.Label, Cmd: e.Cmd, PID: e.PID,
		Position: e.Position, AgeMS: e.Age.Milliseconds(), ETAMS: e.ETA.Milliseconds()}
}

// KillHeavyLease removes a heavy-run lease by token or prefix, as saddle
// runq kill does: a holder's command keeps running but loses its slot.
func (a *App) KillHeavyLease(token string) error {
	q, _, err := a.Heavy().Open()
	if err != nil {
		return err
	}
	defer func() { _ = q.Close() }()
	return q.Kill(token)
}
