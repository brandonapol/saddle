package app

import (
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/tmux"
	"github.com/brandonapol/saddle/internal/usage"
)

// Resuming workers parked on a usage limit (#480). Claude Code continues on
// its own after the reset; Codex and Grok end the turn on the error and wait,
// so once their adapter's reset has passed saddle types a continue line.

// LimitResumeLine is typed into a parked worker whose CLI doesn't resume on
// its own, once its adapter's quota is back.
const LimitResumeLine = "[saddle] Your usage limit has reset. Continue where you left off."

// LimitAutoResumes reports whether task's CLI continues by itself after a
// usage-limit reset.
func (a *App) LimitAutoResumes(task string) bool {
	t, err := a.Store.Task(task)
	if err != nil {
		return true
	}
	return agent.AutoResumes(a.taskAdapter(t).Name())
}

// ResumeAfterLimit types LimitResumeLine into task when its CLI doesn't
// resume on its own and its adapter is no longer out of quota at now. b is
// the banner its screen shows; a worker already typed past that banner is
// left alone. It reports whether it typed.
func (a *App) ResumeAfterLimit(task string, b usage.LimitBanner, now time.Time) (bool, error) {
	t, err := a.Store.Task(task)
	if err != nil {
		return false, err
	}
	ad := a.taskAdapter(t)
	if agent.AutoResumes(ad.Name()) || !a.ownWindow(t) || a.ResumedPast(task, b) {
		return false, nil
	}
	if _, out := quotaOut(a.ExhaustedAdapters(now), a.ExhaustedModels(now), ad.Name(), t.Model); out {
		return false, nil
	}
	w := a.wakeState()
	w.mu.Lock()
	if r, ok := w.resumed[task]; ok && r == b.Resets {
		w.mu.Unlock()
		return false, nil // another caller got here first
	}
	w.resumed[task] = b.Resets
	w.mu.Unlock()
	ok, err := tmux.Submit(a.Tmux, t.Window, LimitResumeLine)
	if err != nil {
		return false, err
	}
	how := "typed and submitted"
	if !ok {
		how = "typed, but the input box never emptied"
	}
	a.Store.Event(task, "usage_resume_typed", how)
	return true, nil
}

// ResumedPast reports whether saddle already typed task past banner b: the
// same banner still in its scrollback is not a new limit.
func (a *App) ResumedPast(task string, b usage.LimitBanner) bool {
	w := a.wakeState()
	w.mu.Lock()
	defer w.mu.Unlock()
	r, ok := w.resumed[task]
	return ok && r == b.Resets
}

// ForgetResumed drops the banner task was typed past: its screen no longer
// shows one, so the next banner is a new limit.
func (a *App) ForgetResumed(task string) {
	w := a.wakeState()
	w.mu.Lock()
	delete(w.resumed, task)
	w.mu.Unlock()
}
