package app

import (
	"regexp"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// Hookless idle detection (#480). Claude and grok report a finished turn
// through their Stop hook; codex and gemini have none, so their status would
// stay running after they stop. Their screen stands in: one that sits still
// with no working indicator is idle, and one showing the indicator again is
// running.

// HooklessIdleAfter is how long a hookless worker's screen must sit
// unchanged, with no working indicator, before it counts as idle.
var HooklessIdleAfter = 20 * time.Second

// workingRe is a CLI's "turn in progress" line: codex's "• Working (3s • esc
// to interrupt)", gemini's "(esc to cancel, 3s)".
var workingRe = regexp.MustCompile(`(?i)esc to (interrupt|cancel)`)

// ObserveHooklessScreen updates a hookless worker's status from its screen:
// still for HooklessIdleAfter with nothing working marks it idle; working
// marks it running. still is how long the screen has sat unchanged. Hooked
// workers, prompts and statuses other than running or idle are left alone.
func (a *App) ObserveHooklessScreen(task, status string, still time.Duration, screen string) {
	working := workingRe.MatchString(screen)
	var next string
	switch {
	case status == store.Idle && working:
		next = store.Running
	case status == store.Running && !working && still >= HooklessIdleAfter:
		next = store.Idle
	default:
		return
	}
	t, err := a.Store.Task(task)
	if err != nil || t.Role != store.RoleWorker || t.Status != status || a.taskAdapter(t).Hooks() || DetectPrompt(screen) != PromptNone {
		return
	}
	if err := a.Store.SetStatus(task, next); err == nil {
		a.Store.Event(task, "screen_"+next, "hookless agent's screen")
	}
}
