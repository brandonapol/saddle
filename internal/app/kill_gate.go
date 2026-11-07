package app

import (
	"context"
	"errors"
	"time"
)

// Kill and a running done gate are usually in different processes: the
// gate runs in the agent's MCP server or its `saddle done`, Kill in the
// orchestrator's CLI. So Kill cancels the gate through shared state (#849):
// it marks the task killed in the store, which the gate polls, and removes
// the task's heavy-run leases, whose holders' Lost channels close.

// killPoll is how often a running done gate checks whether its task was
// killed.
var killPoll = 250 * time.Millisecond

// untilKilled is ctx, also cancelled once task is killed (or otherwise
// finished) by any process. Call stop when the gate is over.
func (a *App) untilKilled(ctx context.Context, task string) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(killPoll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if a.finishedTask(task) {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, cancel
}

// killTaskLeases removes this repo's heavy-run leases labeled task, held or
// waiting, so a killed task's gate stops holding a slot. A queue it can't
// read is left alone: the reaper frees a dead holder's lease anyway.
func (a *App) killTaskLeases(task string) {
	v, err := a.HeavyRuns()
	if err != nil {
		return
	}
	for _, c := range v.Classes {
		for _, e := range append(c.Holders, c.Waiters...) {
			if e.Task != task || e.Repo != v.Repo {
				continue
			}
			_ = a.KillHeavyLease(e.Lease)
		}
	}
}

// ErrGateKilled is a done gate stopped because its task was killed (#849).
var ErrGateKilled = errors.New("gate stopped: the task was killed")

// EventDoneGateKilled is a done gate stopped because its task was killed.
// Data: the gate command.
const EventDoneGateKilled = "done_gate_killed"
