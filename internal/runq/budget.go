package runq

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// The machine budget (docs/resource-queue.md): every class's running leases
// share Options.Budget units, and a run of class C uses WeightFor(C) of
// them. It sits on top of the per-class slots: a run needs both a free slot
// in its class and room in the budget.

// Budget reports the units running leases use and the machine's budget
// (< 0 when the budget is off).
func (q *Queue) Budget() (used, budget int, err error) {
	err = q.tx(func(tx *sql.Tx) error {
		if err := q.reap(tx); err != nil {
			return err
		}
		used, err = runningWeight(tx)
		return err
	})
	return used, q.opts.Budget, err
}

func runningWeight(tx *sql.Tx) (int, error) {
	var used int
	err := tx.QueryRow(`SELECT COALESCE(SUM(weight), 0) FROM leases WHERE state='running'`).Scan(&used)
	return used, err
}

// budgetAdmits reports whether token's run fits in the budget now. Waiters
// whose class has a free slot for them are walked in rank order (aged
// priority, then arrival) across every class, each reserving its weight.
// The first that doesn't fit stops the walk, so lighter runs behind a heavy
// waiter can't starve it. A run heavier than the whole budget fits only when
// nothing else holds or has reserved a unit.
func (q *Queue) budgetAdmits(tx *sql.Tx, token string, now time.Time) (bool, string, error) {
	if q.opts.Budget < 0 {
		return true, "", nil
	}
	used, err := runningWeight(tx)
	if err != nil {
		return false, "", err
	}
	type waiter struct {
		token, class, label string
		seq                 int64
		prio, weight        int
	}
	rows, err := tx.Query(`SELECT seq, token, class, label, prio, enqueued, weight FROM leases WHERE state='waiting'`)
	if err != nil {
		return false, "", err
	}
	var ws []waiter
	for rows.Next() {
		var w waiter
		var enq int64
		if err := rows.Scan(&w.seq, &w.token, &w.class, &w.label, &w.prio, &enq, &w.weight); err != nil {
			rows.Close()
			return false, "", err
		}
		w.prio += int(now.Sub(time.UnixMilli(enq)) / q.opts.AgingStep)
		ws = append(ws, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, "", err
	}
	sort.SliceStable(ws, func(i, j int) bool {
		if ws[i].prio != ws[j].prio {
			return ws[i].prio > ws[j].prio
		}
		return ws[i].seq < ws[j].seq
	})
	// A waiter is in the running for budget only once its class has a slot
	// for it: free slots are handed out in the same rank order.
	free := map[string]int{}
	for _, w := range ws {
		if _, ok := free[w.class]; ok {
			continue
		}
		n, err := q.freeSlots(tx, w.class)
		if err != nil {
			return false, "", err
		}
		free[w.class] = n
	}
	avail, reserved := q.opts.Budget-used, 0
	for _, w := range ws {
		if free[w.class] <= 0 {
			continue
		}
		free[w.class]--
		if w.weight > avail && (used > 0 || reserved > 0) {
			if w.token == token {
				return false, fmt.Sprintf("%d/%d units busy, %s needs %d", used, q.opts.Budget, w.class, w.weight), nil
			}
			return false, fmt.Sprintf("%d/%d units busy, behind %s (%s, %d units)", used, q.opts.Budget, w.label, w.class, w.weight), nil
		}
		if w.token == token {
			return true, "", nil
		}
		avail -= w.weight
		reserved += w.weight
	}
	// Not among the eligible waiters (its class is full): the slot check
	// already holds it.
	return false, "", nil
}

// freeSlots is class's slots minus its running leases.
func (q *Queue) freeSlots(tx *sql.Tx, class string) (int, error) {
	slots := q.slotsFor(class)
	if err := tx.QueryRow(`SELECT slots FROM classes WHERE name=?`, class).Scan(&slots); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	var running int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM leases WHERE class=? AND state='running'`, class).Scan(&running); err != nil {
		return 0, err
	}
	return slots - running, nil
}
