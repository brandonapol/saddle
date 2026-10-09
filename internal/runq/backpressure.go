package runq

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Backpressure (docs/runq.md Q6): when the heavy-run queue is backed up, a
// new worker would only queue for CPU, so spawn refuses until it drains.

// DefaultBackpressureWait is how long a class's oldest waiter may wait
// before spawns hold off; [runq] backpressure_wait overrides it.
const DefaultBackpressureWait = 10 * time.Minute

// backpressureFactor: spawns hold off when a class has more than this many
// waiters per slot.
const backpressureFactor = 2

// BackpressureLimit is backpressure_wait, else DefaultBackpressureWait. A
// negative value turns the age check off.
func (c Config) BackpressureLimit() time.Duration {
	if c.BackpressureWait.D == 0 {
		return DefaultBackpressureWait
	}
	return c.BackpressureWait.D
}

// Backlog is one class's queue as backpressure sees it. Gate runs (base
// priority PrioGate or more) don't count: landing frees capacity.
type Backlog struct {
	Class   string
	Slots   int
	Waiting int           // non-gate waiters
	Oldest  time.Duration // how long the oldest of them has waited
	Median  time.Duration // the class's median run time; 0 without history
}

// ETA is roughly how long the backlog takes to clear: the median run time
// per round of slots. 0 without history.
func (b Backlog) ETA() time.Duration {
	if b.Slots <= 0 {
		return 0
	}
	return b.Median * time.Duration((b.Waiting+b.Slots-1)/b.Slots)
}

// Pressure is a backed-up class and why it counts as backed up.
type Pressure struct {
	Backlog
	Why string
}

func (p Pressure) String() string {
	eta := "no run-time history for an ETA"
	if d := p.ETA(); d > 0 {
		eta = "clears in ~" + roughDuration(d)
	}
	return fmt.Sprintf("%s has %d runs waiting for %s (oldest %s, %s): %s",
		p.Class, p.Waiting, plural(p.Slots, "slot"), roughDuration(p.Oldest), eta, p.Why)
}

func plural(n int, s string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, s)
	}
	return fmt.Sprintf("%d %ss", n, s)
}

// Backpressure reports the most backed-up class, or nil when none is: one
// with more than 2 × slots waiters, or whose oldest waiter has waited longer
// than maxWait (maxWait <= 0 turns that check off). A drained class (0
// slots) never counts: the owner stopped it on purpose, and drain is one of
// the escape hatches.
func Backpressure(bs []Backlog, maxWait time.Duration) *Pressure {
	var worst *Pressure
	for _, b := range bs {
		if b.Slots <= 0 || b.Waiting == 0 {
			continue
		}
		var why string
		switch {
		case b.Waiting > backpressureFactor*b.Slots:
			why = fmt.Sprintf("more than %d× its %s", backpressureFactor, plural(b.Slots, "slot"))
		case maxWait > 0 && b.Oldest > maxWait:
			why = fmt.Sprintf("its oldest waiter has waited %s (limit %s)", roughDuration(b.Oldest), roughDuration(maxWait))
		default:
			continue
		}
		if worst == nil || b.Oldest > worst.Oldest {
			worst = &Pressure{Backlog: b, Why: why}
		}
	}
	return worst
}

// Backlogs reaps dead leases and reports each class's non-gate waiters.
// Classes with none are left out.
func (q *Queue) Backlogs() ([]Backlog, error) {
	var out []Backlog
	err := q.tx(func(tx *sql.Tx) error {
		if err := q.reap(tx); err != nil {
			return err
		}
		rows, err := tx.Query(`SELECT class, COUNT(*), MIN(enqueued) FROM leases
			WHERE state='waiting' AND prio < ? GROUP BY class ORDER BY class`, PrioGate)
		if err != nil {
			return err
		}
		now := q.opts.Now()
		for rows.Next() {
			var b Backlog
			var enq int64
			if err := rows.Scan(&b.Class, &b.Waiting, &enq); err != nil {
				rows.Close()
				return err
			}
			b.Oldest = now.Sub(time.UnixMilli(enq))
			out = append(out, b)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range out {
			b := &out[i]
			b.Slots = q.slotsFor(b.Class)
			if err := tx.QueryRow(`SELECT slots FROM classes WHERE name=?`, b.Class).Scan(&b.Slots); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if b.Median, err = medianHeld(tx, b.Class); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}
