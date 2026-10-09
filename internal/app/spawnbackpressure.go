package app

import (
	"errors"
	"fmt"

	"github.com/brandonapol/saddle/internal/runq"
	"github.com/brandonapol/saddle/internal/store"
)

// ErrBackpressure is spawn's refusal while the heavy-run queue is backed up
// (#243, docs/runq.md Q6): a new worker would only queue for CPU.
var ErrBackpressure = errors.New("heavy-run queue backed up")

// EventSpawnBackpressure logs each spawn backpressure refused.
const EventSpawnBackpressure = "spawn_backpressure"

// Backpressure reports the most backed-up heavy-run class, or nil when the
// queue has room. Gate runs never count. The usage-limit logic (#40) can
// read the same signal. A queue that is off or can't be read reports nil:
// backpressure fails open.
func (a *App) Backpressure() *runq.Pressure {
	q, cfg, err := a.Heavy().Open()
	if err != nil {
		return nil
	}
	defer func() { _ = q.Close() }()
	if q.Mode() == runq.ModeOff {
		return nil
	}
	bs, err := q.Backlogs()
	if err != nil {
		return nil
	}
	return runq.Backpressure(bs, cfg.BackpressureLimit())
}

// checkBackpressure refuses a spawn while the heavy-run queue is backed up,
// and tells the orchestrator. Spawn's Force skips it.
func (a *App) checkBackpressure() error {
	p := a.Backpressure()
	if p == nil {
		return nil
	}
	a.Store.Event(OrchestratorID, EventSpawnBackpressure, p.String())
	// No numbers here, so repeats within the digest window fold into one.
	_ = a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf(
		"Spawns are on hold: the heavy-run queue for %s is backed up, so new workers would only wait for CPU. They resume once it drains; `saddle spawn --force` overrides.", p.Class))
	return fmt.Errorf("%w: %s. A new worker would only queue for CPU; spawn again once it drains (`saddle runq status` shows it), or override with `saddle spawn --force`", ErrBackpressure, p)
}
