package app

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/tmux"
)

// wakeState remembers when each task was last force-woken, and which
// hookless tasks have notices on their way into the pane (by generation, so
// a stale timeout can't release a later injection).
type wakeState struct {
	mu        sync.Mutex
	woke      map[string]time.Time
	injecting map[string]int
	gen       int
}

func (a *App) wakeState() *wakeState {
	a.wake.mu.Lock()
	defer a.wake.mu.Unlock()
	if a.wake.woke == nil {
		a.wake.woke, a.wake.injecting = map[string]time.Time{}, map[string]int{}
	}
	return &a.wake
}

// waitsAtPrompt reports whether t is an agent sitting at its input that a
// wake line should reach: idle, done, back from the train with a conflict
// or failed tests, or escalated (#189).
func waitsAtPrompt(t store.Task) bool {
	switch t.Status {
	case store.Idle, store.Done, store.Conflict, store.NeedsYou:
		return true
	}
	return false
}

// WakeIdle wakes every worker that waits at its prompt with action notices
// queued longer than notices.wake_after: the grey-suggestion check or a
// stray draft kept them out (#183). The input line is cleared and the wake
// line typed. Agents on a permission prompt are left alone, and each task
// is woken at most once per wake_after. It returns the tasks woken.
func (a *App) WakeIdle(now time.Time) ([]string, error) {
	after := a.Cfg.Notices.WakeAfter
	ts, err := a.Store.Tasks()
	if err != nil {
		return nil, err
	}
	w := a.wakeState()
	var woke []string
	for _, t := range ts {
		if !t.Active() || !waitsAtPrompt(t) {
			continue
		}
		since, ok, err := a.Store.OldestPendingAction(t.ID)
		if err != nil || !ok || now.Sub(since) < after {
			continue
		}
		w.mu.Lock()
		last := w.woke[t.ID]
		w.mu.Unlock()
		if !last.IsZero() && now.Sub(last) < after {
			continue
		}
		if !a.ownWindow(t) {
			continue
		}
		screen, err := a.Tmux.Capture(t.Window, 30)
		if err != nil || DetectPrompt(screen) != PromptNone {
			continue
		}
		input := tmux.InputEmpty
		if s, ok := a.Tmux.(tmux.StyledCapturer); ok {
			if styled, err := s.CaptureStyled(t.Window, 15); err == nil {
				input = tmux.ReadInput(styled)
			}
		} else {
			input = tmux.ReadInput(screen)
		}
		w.mu.Lock()
		w.woke[t.ID] = now
		w.mu.Unlock()
		a.Store.Event(t.ID, "notice_wake", fmt.Sprintf("action notices waited %s; input was %s", now.Sub(since).Round(time.Second), input))
		if ad := a.taskAdapter(t); !ad.Hooks() {
			_ = a.injectNotices(t, ad, true)
		} else {
			a.sendWake(t, true)
		}
		woke = append(woke, t.ID)
	}
	return woke, nil
}

// RunNoticeWaker runs WakeIdle every 30 seconds until ctx ends.
func (a *App) RunNoticeWaker(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			_, _ = a.WakeIdle(now)
		}
	}
}

// sendWake types the wake line into a hooked agent, whose hook then hands
// it its notices. force types over what looks like a draft.
func (a *App) sendWake(t store.Task, force bool) {
	asks := t.Status == store.NeedsYou
	tmux.Deliver(a.Tmux, t.Window, agent.Claude{}.Inject(""), tmux.Delivery{
		Force: force,
		Needed: func() bool {
			if asks && a.onPrompt(t) {
				return false
			}
			n, err := a.Store.PendingNotices(t.ID)
			return err != nil || n > 0
		},
	})
}
