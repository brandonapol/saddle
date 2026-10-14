package app

import (
	"regexp"
	"time"

	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

// Hookless idle detection (#480). Claude and grok report a finished turn
// through their Stop hook; codex and gemini have none, so their status would
// stay running after they stop. Their screen stands in: one that sits still
// with no working indicator is idle, and one showing the indicator again is
// running.

// HooklessIdleAfter is how long a hookless worker's screen must sit
// unchanged, with no working indicator, before it counts as idle.
var HooklessIdleAfter = 20 * time.Second

// What a hookless worker's pane is doing (#520).
const (
	screenBusy     = "busy"     // a turn is in progress
	screenIdle     = "idle"     // sitting on the composer
	screenApproval = "approval" // a permission panel
	screenQuota    = "quota"    // a usage-limit banner
)

// workingRe is a CLI's "turn in progress" line: codex's "• Working (3s • esc
// to interrupt)", gemini's "(esc to cancel, 3s)", grok's "Ctrl+c:cancel"
// footer (#488), opencode's "esc interrupt" (#520).
var workingRe = regexp.MustCompile(`(?i)esc to (interrupt|cancel)|esc interrupt|Ctrl\+c:cancel`)

// classifyHooklessScreen reads a captured pane. A usage-limit banner is
// quota even when the footer is quiet; an approval panel is approval; a
// working footer is busy; anything else is idle.
func classifyHooklessScreen(screen string) string {
	if _, on := usage.DetectLimitBanner(screen); on {
		return screenQuota
	}
	if DetectPrompt(screen) != PromptNone {
		return screenApproval
	}
	if workingRe.MatchString(screen) {
		return screenBusy
	}
	return screenIdle
}

// ObserveHooklessScreen updates a hookless worker's status from its screen:
// still for HooklessIdleAfter with nothing working marks it idle; working
// marks it running. still is how long the screen has sat unchanged. Hooked
// workers, prompts, quota banners and statuses other than running or idle
// are left alone.
func (a *App) ObserveHooklessScreen(task, status string, still time.Duration, screen string) {
	var next string
	switch classifyHooklessScreen(screen) {
	case screenQuota, screenApproval:
		return
	case screenBusy:
		if status != store.Idle {
			return
		}
		next = store.Running
	case screenIdle:
		if status != store.Running || still < HooklessIdleAfter {
			return
		}
		next = store.Idle
	default:
		return
	}
	t, err := a.Store.Task(task)
	if err != nil || t.Role != store.RoleWorker || t.Status != status || a.taskAdapter(t).Hooks() {
		return
	}
	if err := a.Store.SetStatus(task, next); err == nil {
		a.Store.Event(task, "screen_"+next, "hookless agent's screen")
	}
}
