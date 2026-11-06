package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// Prompt waits (#786). A worker in needs-you sits on a permission prompt or
// a question, and the hung-worker and done-orphan watches leave it alone:
// someone at the TUI answers those. Overnight nobody does, and the worker
// holds its slot and claims until morning. The prompt-wait watch gives
// needs-you a timeout: once a needs-you worker made no progress (screen,
// branch, working tree) for [stall] hung_after, it opens a card whose
// default, run on its own, presses Escape and tells the agent the action
// isn't allowed here. If the same wait outlasts a second timeout, the
// default parks the task (paused, window closed, slot freed, resumed by
// itself later), as the risky-prompt card does. From the moment its card
// opens, the task no longer counts toward the concurrency cap.

// DecisionPromptWait is the card for a worker left waiting on a prompt.
const DecisionPromptWait = "prompt-wait"

// promptWaitNote is what a worker whose prompt timed out is told.
const promptWaitNote = "Nobody answered your prompt: that action isn't allowed here. Find another way, or call done with what you have and say what you needed."

// PromptWait is one needs-you worker the watch is timing.
type PromptWait struct {
	Task    string    `json:"task"`
	Episode time.Time `json:"episode"`           // it went into needs-you
	Print   string    `json:"print"`             // its progress fingerprint when last seen
	Since   time.Time `json:"since"`             // the fingerprint has not changed since
	Card    string    `json:"card,omitempty"`    // its card, once the wait ran out
	Overdue bool      `json:"overdue,omitempty"` // past its timeout: it no longer holds a slot
}

func (a *App) promptWaitPath() string { return a.stateDir("prompt-waits.json") }

func (a *App) readPromptWaits() (map[string]PromptWait, error) {
	m := map[string]PromptWait{}
	b, err := os.ReadFile(a.promptWaitPath())
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]PromptWait{}, fmt.Errorf("%s: %w", a.promptWaitPath(), err)
	}
	return m, nil
}

func (a *App) writePromptWaits(m map[string]PromptWait) error {
	if len(m) == 0 {
		if err := os.Remove(a.promptWaitPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.promptWaitPath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.promptWaitPath())
}

// PromptOverdue lists the needs-you workers whose wait ran out: they don't
// count toward the concurrency cap.
func (a *App) PromptOverdue() map[string]bool {
	m, _ := a.readPromptWaits()
	out := map[string]bool{}
	for id, w := range m {
		if w.Overdue {
			out[id] = true
		}
	}
	return out
}

// PromptWaitTick runs the prompt-wait watch at now. It returns the tasks
// whose card it opened. [stall] hung_after sets the timeout; a negative one
// turns the watch off.
func (a *App) PromptWaitTick(now time.Time) ([]string, error) {
	window := a.StallConfig().HungAfter
	if window < 0 {
		return nil, a.writePromptWaits(nil)
	}
	ts, err := a.Store.Tasks()
	if err != nil {
		return nil, err
	}
	m, err := a.readPromptWaits()
	if err != nil {
		return nil, err
	}
	open, err := a.OpenDecisions()
	if err != nil {
		return nil, err
	}
	var acted []string
	var errs []error
	seen := map[string]bool{}
	for _, t := range ts {
		if t.Role != store.RoleWorker || t.Status != store.NeedsYou || !a.ownWindow(t) {
			continue // not waiting, or no window: ResumeLost's, not ours
		}
		seen[t.ID] = true
		w, had := m[t.ID]
		p := a.progressPrint(t)
		if !had {
			m[t.ID] = PromptWait{Task: t.ID, Episode: now.UTC(), Print: p, Since: now.UTC()}
			continue
		}
		ran := w.Card != "" && !slices.ContainsFunc(open, func(c DecisionCard) bool { return c.ID == w.Card })
		if ran || w.Print != p {
			// It moved, or its card closed (its default ran): time it afresh,
			// but in the same episode, so a second timeout parks it.
			w.Print, w.Since = p, now.UTC()
			if ran {
				w.Card = ""
			}
			m[t.ID] = w
			continue
		}
		if now.Sub(w.Since) < window {
			continue
		}
		w.Overdue = true
		carded := slices.ContainsFunc(open, func(c DecisionCard) bool {
			return c.Task == t.ID && (c.Kind == DecisionRiskyPrompt || c.Kind == DecisionPromptWait)
		})
		if !carded {
			c := promptWaitCard(t, w, now, window)
			if err := a.OpenDecision(c); err != nil {
				errs = append(errs, err)
			} else {
				w.Card = c.ID
				a.Store.Event(t.ID, "prompt_wait", "no answer for "+fmtDuration(now.Sub(w.Since)))
				acted = append(acted, t.ID)
			}
		}
		m[t.ID] = w
	}
	for id, w := range m {
		if !seen[id] {
			// Out of needs-you: the episode is over, and so is its card.
			if w.Card != "" {
				errs = append(errs, a.ResolveDecision(w.Card, now))
			}
			delete(m, id)
		}
	}
	errs = append(errs, a.writePromptWaits(m))
	return acted, errors.Join(errs...)
}

// promptWaitCard is the card for t, left on a prompt since w.Since. Its ID
// is per needs-you episode, so the same wait timing out twice parks it.
func promptWaitCard(t store.Task, w PromptWait, now time.Time, window time.Duration) DecisionCard {
	return DecisionCard{
		ID:       DecisionPromptWait + ":" + t.ID + "@" + strconv.FormatInt(w.Episode.Unix(), 10),
		Kind:     DecisionPromptWait,
		To:       OrchestratorID,
		Task:     t.ID,
		Headline: fmt.Sprintf("%s has waited on a prompt for %s and nobody answered.", taskLabel(t), fmtDuration(now.Sub(w.Since))),
		What:     "It sits on a permission prompt or a question; its screen, branch and working tree haven't moved. It no longer counts toward the concurrency cap.",
		Blocking: "Only " + t.ID + ", which waits on the prompt and holds its claims. Everything else keeps running.",
		Options: []DecisionOption{
			{Label: "Answer the prompt yourself", Cost: "you, a minute", Action: "saddle peek " + t.ID + ", then send_keys " + t.ID + " <keys>"},
			{Label: "Deny it and tell the agent to find another way", Cost: "it keeps working without it; if it is still waiting after another " + fmtDuration(window) + ", saddle parks it and resumes it later",
				Action: "send_keys " + t.ID + " Escape, then saddle message " + t.ID + " \"" + promptWaitNote + "\"", Recommended: true, Run: RunDenyPrompt},
			{Label: "Stop the task", Cost: "its work so far stays on its branch", Action: "saddle kill " + t.ID},
		},
		Default: 2,
		Timeout: window,
		Opened:  now,
	}
}
