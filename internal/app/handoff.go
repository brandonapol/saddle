package app

import (
	"fmt"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

// Mid-task failover (#481): a worker whose adapter runs out of quota is
// handed to the next adapter with quota instead of waiting for the reset.
// The finisher runs in the same worktree on the same branch, so it starts
// from every commit and uncommitted change the first agent left. It parks
// as before when [adapters] failover = "park", when the reset is within
// handoff_wait, or when no other adapter has quota.

const (
	EventAdapterHandoff = "adapter_handoff"
	EventHandoffSkipped = "adapter_handoff_skipped"
)

// handoffNote tells the finisher it is taking over another agent's work.
func handoffNote(from, to, integration string) string {
	return fmt.Sprintf("[saddle] You are taking over this task: the agent working on it ran out of quota on %s, so saddle relaunched it on %s. "+
		"This worktree and branch hold its work so far, commits and uncommitted changes alike. "+
		"Start with git status and git log %s..HEAD to see what it did, then finish the task; don't redo work that is already there.",
		from, to, integration)
}

// LimitHit handles a worker whose screen shows a usage-limit banner: its
// adapter is marked out of quota until the reset (#321) and, by [adapters]
// failover, the task is handed to another adapter (#481). It returns the
// adapter that took over, or "" when the worker stays parked.
func (a *App) LimitHit(task string, b usage.LimitBanner) (string, error) {
	if err := a.AdapterLimitHit(task, b); err != nil {
		return "", err
	}
	return a.HandOff(task, time.Now())
}

// HandOff relaunches task on the best adapter with quota when its own is
// out at now and the policy says hand off. It returns the new adapter, or
// "" when the task stays where it is.
func (a *App) HandOff(task string, now time.Time) (string, error) {
	p := a.Cfg.AdapterPolicy
	if p.Failover != config.FailoverHandoff {
		return "", nil
	}
	t, err := a.Store.Task(task)
	if err != nil {
		return "", err
	}
	if t.Role != store.RoleWorker || !t.Active() || t.Status == store.Done {
		return "", nil
	}
	// Only a worker in a window saddle runs can be stopped before its
	// finisher starts in the same worktree.
	if a.Tmux == nil || !a.ownWindow(t) {
		return "", nil
	}
	from := a.taskAdapter(t).Name()
	all, err := a.Store.Claims()
	if err != nil {
		return "", err
	}
	// Its adapter, or its model when [fallback] tracks that on its own
	// (#483), is out: the chain, else the policy, picks the finisher.
	c, e, out, rerr := a.reroute(from, t.Model, pickReq{claims: len(all[t.ID]) > 0}, now)
	if !out {
		return "", nil
	}
	if p.HandoffWait > 0 && e.Until.Sub(now) <= p.HandoffWait {
		a.Store.Event(task, EventHandoffSkipped, fmt.Sprintf("%s resets at %s, within handoff_wait %s", e.Label(), e.when(), p.HandoffWait))
		return "", nil
	}
	if rerr != nil {
		a.Store.Event(task, EventHandoffSkipped, "no other adapter has quota")
		return "", nil
	}
	ad, err := agent.ByName(c.Adapter)
	if err != nil {
		return "", err
	}
	// The worktree stays as it is for the finisher; the snapshot is a
	// safety net in case the old agent's exit touches it.
	a.snapshotWIP(t.ID, t.Worktree)
	if err := a.Tmux.KillWindow(t.Window); err != nil {
		a.Store.Event(task, "handoff_kill_failed", err.Error())
	}
	t.Model, t.Adapter = c.Model, c.Adapter
	if err := a.Store.SetField(t.ID, "model", t.Model); err != nil {
		return "", err
	}
	if err := a.Store.SetField(t.ID, "adapter", t.Adapter); err != nil {
		return "", err
	}
	prompt := strings.TrimSpace(t.Prompt + "\n\n" + handoffNote(e.Label(), c.Label(), a.Cfg.Integration))
	win, err := a.start(t, all[t.ID], ad, "", prompt)
	if err != nil {
		// Its window is gone; ResumeLost relaunches it on an adapter with quota.
		_ = a.Store.SetField(t.ID, "window", "")
		a.Store.Event(task, "handoff_failed", err.Error())
		return "", fmt.Errorf("hand %s to %s: %w", task, c.Label(), err)
	}
	if err := a.Store.SetStatus(t.ID, store.Running); err != nil {
		return "", err
	}
	a.ForgetResumed(task)
	a.Store.Event(task, EventAdapterHandoff, fmt.Sprintf("%s -> %s (%s) window %s", e.Label(), c.Label(), c.Why, win))
	text := fmt.Sprintf("%s %q ran out of quota on %s (resets %s), so saddle handed it to %s: a new %s agent finishes it in the same worktree and branch. Nothing needs doing.",
		t.ID, t.Title, e.Label(), e.when(), c.Label(), c.Adapter)
	if cl := all[t.ID]; len(cl) > 0 && !ad.Hooks() {
		text += fmt.Sprintf(" %s has no hooks, so saddle checks its claims (%s) only when it commits and calls done.", c.Adapter, strings.Join(cl, ", "))
	}
	return c.Adapter, a.Notify(OrchestratorID, store.NoticeInfo, text)
}
