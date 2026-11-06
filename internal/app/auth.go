package app

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/store"
)

// Expired adapter logins (#798). Availability only checks that a
// credentials file exists, and an expired or revoked OAuth login keeps its
// file, so spawns kept routing to the adapter and its agents sat at a login
// screen until the hung watchdog or a human found them. Now a login screen
// on any worker marks its adapter unauthenticated: out like an adapter out
// of quota, but with no timer. New spawns route past it, its running tasks
// are handed off, and it stays out until a successful login clears it
// (saddle doctor --fix probes the login and does, as does a finished SSO
// sign-in).

// EventAdapterUnauthenticated records an adapter marked logged out; data:
// the adapter and why.
const EventAdapterUnauthenticated = "adapter_unauthenticated"

// AuthHit handles a worker whose screen shows its adapter's login prompt or
// a 401: the adapter is marked unauthenticated and every running task on it
// is handed to an adapter that can run.
func (a *App) AuthHit(task string) error {
	t, err := a.Store.Task(task)
	if err != nil {
		return err
	}
	ad := a.taskAdapter(t).Name()
	return a.MarkUnauthenticated(ad, fmt.Sprintf("%s's screen shows its login prompt", task))
}

// MarkUnauthenticated marks adapter logged out until ClearUnauthenticated,
// tells the owner once how to log back in, and hands off its running
// tasks. Marking an adapter already logged out only retries the handoffs.
func (a *App) MarkUnauthenticated(adapter, why string) error {
	return a.markUnauthenticated(adapter, why, true)
}

// markUnauthenticated is MarkUnauthenticated; tell false leaves the owner's
// page to the caller (an SSO sign-in pages with its code).
func (a *App) markUnauthenticated(adapter, why string, tell bool) error {
	now := time.Now()
	exhaustMu.Lock()
	m := a.readExhausted()
	prev, had := m[adapter]
	fresh := !had || prev.Reason != ReasonLogin
	if fresh {
		m[adapter] = Exhaustion{Adapter: adapter, Since: now, Reason: ReasonLogin}
	}
	err := a.writeExhausted(m)
	exhaustMu.Unlock()
	if err != nil {
		return err
	}
	var errs []error
	if fresh {
		a.Store.Event(OrchestratorID, EventAdapterUnauthenticated, adapter+": "+why)
	}
	if fresh && tell {
		next := "nothing else can run"
		if n, err := a.nextAdapter(adapter, a.ExhaustedAdapters(now)); err == nil {
			next = n
		}
		errs = append(errs, a.tellOwner(fmt.Sprintf(
			"Adapter %s is logged out (%s): its login expired or was revoked. New spawns that would use it go to %s instead, and its running tasks are handed off to finish in their worktrees. "+
				"Run %s, then saddle doctor --fix to check the login and put %s back.",
			adapter, why, next, agent.LoginHint(adapter), adapter)))
	}
	errs = append(errs, a.handOffAdapter(adapter, now))
	return errors.Join(errs...)
}

// handOffAdapter hands every running worker on adapter to another adapter.
func (a *App) handOffAdapter(adapter string, now time.Time) error {
	ts, err := a.Store.Tasks()
	if err != nil {
		return err
	}
	var errs []error
	for _, t := range ts {
		if t.Role != store.RoleWorker || !t.Active() || a.taskAdapter(t).Name() != adapter {
			continue
		}
		if _, err := a.HandOff(t.ID, now); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ClearUnauthenticated puts adapter back after a successful login. An
// adapter out of quota stays out.
func (a *App) ClearUnauthenticated(adapter string) error {
	exhaustMu.Lock()
	e, out := a.readExhausted()[adapter]
	exhaustMu.Unlock()
	if !out || e.Reason != ReasonLogin {
		return nil
	}
	return a.ClearExhausted(adapter)
}

// OutUntilCleared lists the adapters out with no timer: logged out or out
// of credits, the ones saddle doctor --fix probes, sorted.
func (a *App) OutUntilCleared() []Exhaustion {
	var out []Exhaustion
	for _, e := range a.ExhaustedAdapters(time.Now()) {
		if e.Indefinite() {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Adapter < out[j].Adapter })
	return out
}
