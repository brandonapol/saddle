package narrator

import (
	"fmt"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// HeavySource reports the machine's heavy-run queue (#242).
type HeavySource interface {
	HeavyLeases() ([]HeavyLease, error)
}

// HeavyLease is one holder or waiter in the heavy-run queue.
type HeavyLease struct {
	Lease    string
	Class    string
	Who      string // "t83", "other-repo/t84" or "pid 4242"
	Cmd      string
	Holder   bool
	Position int           // waiters: 1 is next
	Age      time.Duration // holders: running for; waiters: waiting for
	MaxRun   time.Duration // holders: the class's max_run
	Overdue  bool          // holders past MaxRun
	Holders  string        // waiters: who holds the class's slots
}

// EventBackpressure is the event kind spawn logs when it refuses because
// the heavy-run queue is backed up (app.EventRunqBackpressure).
const EventBackpressure = "runq_backpressure"

const (
	// HeavyWaitNotice is how long a wait runs before the narrator says so.
	HeavyWaitNotice = 5 * time.Minute
	// heavyEvery is how often the narrator reads the queue.
	heavyEvery = 30 * time.Second
	// backpressureQuiet is how long after mentioning a refusal further
	// refusals stay unmentioned: a backed-up queue refuses every spawn.
	backpressureQuiet = 10 * time.Minute
)

// heavy holds what the narrator has said about the heavy-run queue, so each
// long wait, overdue holder and backpressure spell is mentioned once.
type heavy struct {
	next      time.Time
	said      map[string]bool // "wait <lease>" and "over <lease>"
	refusedAt time.Time
}

// checkHeavy mentions waits past HeavyWaitNotice and overdue holders it
// hasn't mentioned yet. It reads the queue at most every heavyEvery; a read
// error is returned and retried next time.
func (n *Narrator) checkHeavy(now time.Time) error {
	if n.deps.Heavy == nil || now.Before(n.hv.next) {
		return nil
	}
	n.hv.next = now.Add(heavyEvery)
	ls, err := n.deps.Heavy.HeavyLeases()
	if err != nil {
		return fmt.Errorf("narrator: heavy-run queue: %w", err)
	}
	live := map[string]bool{}
	for _, l := range ls {
		var k, text string
		switch {
		case l.Holder && l.Overdue:
			k = "over " + l.Lease
			text = fmt.Sprintf("%s run (%s) is overdue: %s, past max_run %s; saddle runq kill %s stops it",
				l.Class, truncate(l.Cmd, 40), rough(l.Age), rough(l.MaxRun), shortLease(l.Lease))
		case !l.Holder && l.Age >= HeavyWaitNotice:
			k = "wait " + l.Lease
			text = fmt.Sprintf("has waited %s for %s %s slot (position %d", rough(l.Age), article(l.Class), l.Class, l.Position)
			if l.Holders != "" {
				text += ", behind " + l.Holders
			}
			text += ")"
		default:
			continue
		}
		live[k] = true
		if n.hv.said == nil {
			n.hv.said = map[string]bool{}
		}
		if !n.hv.said[k] {
			n.hv.said[k] = true
			n.out(Line{Time: now, Task: l.Who, Text: text})
		}
	}
	for k := range n.hv.said {
		if !live[k] {
			delete(n.hv.said, k)
		}
	}
	return nil
}

// takeBackpressure removes spawn refusals from es and mentions the first
// one, unless one was mentioned in the last backpressureQuiet.
func (n *Narrator) takeBackpressure(now time.Time, es []store.Event) []store.Event {
	kept := es[:0:0]
	for _, e := range es {
		if e.Kind != EventBackpressure {
			kept = append(kept, e)
			continue
		}
		if !n.hv.refusedAt.IsZero() && now.Sub(n.hv.refusedAt) < backpressureQuiet {
			continue
		}
		n.hv.refusedAt = now
		text := "spawn refused: the heavy-run queue is backed up"
		if d := truncate(e.Data, maxNote); d != "" {
			text += " (" + d + ")"
		}
		n.out(Line{Time: now, Task: e.Task, Text: text})
	}
	return kept
}

// rough rounds d for a line: seconds under a minute, else minutes.
func rough(d time.Duration) string {
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	return fmt.Sprintf("%dm", int(d.Round(time.Minute)/time.Minute))
}

func article(word string) string {
	if word != "" && strings.ContainsRune("aeiou", rune(word[0])) {
		return "an"
	}
	return "a"
}

func shortLease(token string) string {
	if len(token) > 8 {
		return token[:8]
	}
	return token
}
