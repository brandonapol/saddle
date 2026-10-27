package app

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

// Orchestrator failover (#481): when the TUI's orchestrator session runs
// out of quota, saddle marks its adapter out, starts a new orchestrator on
// the next adapter with a headless session and quota, and opens it with a
// state summary, since a conversation can't move across adapters. The
// switch is recorded in .saddle/orchestrator-adapter and holds until the
// default adapter has quota again at the next saddle up.

const EventOrchestratorFailover = "orchestrator_failover"

// OrchestratorAdapters are the adapters the TUI's orchestrator can run on:
// the ones with a headless session. Grok, codex and opencode get theirs
// from saddle's bridges, one headless turn per message (#506, #745).
var OrchestratorAdapters = []string{usage.Claude, usage.Grok, usage.Codex, usage.OpenCode}

func (a *App) orchOverridePath() string { return a.stateDir("orchestrator-adapter") }

// orchOverride is the adapter a failover moved the orchestrator to, or "".
func (a *App) orchOverride() string {
	b, err := os.ReadFile(a.orchOverridePath())
	if err != nil {
		return ""
	}
	n := strings.TrimSpace(string(b))
	if !slices.Contains(OrchestratorAdapters, n) {
		return ""
	}
	return n
}

// orchDefault is the orchestrator's adapter without a failover:
// [orchestrator] adapter, grok-bridge under harness = "grok", Claude Code
// when it is installed, else the first headless agent that can run (#668).
func (a *App) orchDefault() string {
	var ss []agent.Status
	if a.Cfg.Orchestrator.Adapter == "" && a.Cfg.Harness != config.HarnessGrok {
		ss = a.adapterStatus()
	}
	return agent.OrchestratorAdapter(a.Cfg, ss)
}

// clearStaleOverride drops the failover once the default adapter has quota
// again, so a fresh saddle up goes back to it.
func (a *App) clearStaleOverride(now time.Time) {
	if a.orchOverride() == "" {
		return
	}
	if _, out := a.ExhaustedAdapters(now)[a.orchDefault()]; out {
		return
	}
	if err := os.Remove(a.orchOverridePath()); err == nil {
		a.Store.Event(OrchestratorID, EventOrchestratorFailover, "back to "+a.orchDefault())
	}
}

// OrchestratorOutOfQuota reports whether text, from a failed turn, a retry
// or the process's exit, says the orchestrator's adapter is out of quota,
// and when it resets.
func OrchestratorOutOfQuota(text string) (usage.LimitBanner, bool) {
	return usage.DetectLimitBanner(text)
}

// markOut marks adapter out of quota until b's reset, or ExhaustFor from
// now when b names none.
func (a *App) markOut(adapter string, b usage.LimitBanner) error {
	now := time.Now()
	until, ok := parseReset(b.Resets, now)
	if !ok {
		until = now.Add(ExhaustFor)
	}
	resets := b.Resets
	if resetInRe.MatchString(resets) {
		resets = "" // "in 2 hours" is stale once read: show the clock time (#480)
	}
	return a.MarkExhausted(adapter, until, resets)
}

// ErrNoOrchestratorAdapter means every adapter the orchestrator can run on
// is out of quota or can't run here.
var ErrNoOrchestratorAdapter = errors.New("no adapter the orchestrator can run on has quota")

// FailOverOrchestrator marks the orchestrator's adapter out of quota until
// b's reset and moves the orchestrator to the best other adapter it can
// run on. It returns the launch for the new session, and the message to
// open it with: who it replaces and the state summary, with recent the
// last lines of the old conversation.
func (a *App) FailOverOrchestrator(b usage.LimitBanner, recent []string) (agent.Launch, string, error) {
	from := a.orchestratorAgent()
	if err := a.markOut(from, b); err != nil {
		return agent.Launch{}, "", err
	}
	now := time.Now()
	ex := a.ExhaustedAdapters(now)
	to := ""
	for _, c := range a.rankAdapters(pickReq{def: from}, ex, now) {
		if c.Rank > 0 && slices.Contains(OrchestratorAdapters, c.Name) {
			to = c.Name
			break
		}
	}
	if to == "" {
		// Ones the policy doesn't list ([adapters] order) still count.
		for _, n := range OrchestratorAdapters {
			if _, out := ex[n]; !out && n != from && slices.Contains(agent.Usable(a.adapterStatus()), n) {
				to = n
				break
			}
		}
	}
	if to == "" {
		first := firstReset(ex)
		a.Store.Event(OrchestratorID, EventOrchestratorFailover, "none: "+from+" out, nothing to switch to")
		return agent.Launch{}, "", fmt.Errorf("%w; the first reset is %s at %s", ErrNoOrchestratorAdapter, first.Adapter, first.when())
	}
	if err := os.MkdirAll(a.stateDir(), 0o755); err != nil {
		return agent.Launch{}, "", err
	}
	if err := os.WriteFile(a.orchOverridePath(), []byte(to+"\n"), 0o644); err != nil {
		return agent.Launch{}, "", err
	}
	// The old session can't be resumed on another adapter.
	if err := a.Store.SetField(OrchestratorID, "session_id", ""); err != nil {
		return agent.Launch{}, "", err
	}
	l, _, err := a.Orchestrator()
	if err != nil {
		return agent.Launch{}, "", err
	}
	a.Store.Event(OrchestratorID, EventOrchestratorFailover, from+" -> "+to)
	return l, a.orchestratorHandoff(from, to, ex[from], recent, now), nil
}

// handoffRecent caps how many old conversation lines the summary carries.
const handoffRecent = 12

// orchestratorHandoff is the first message of an orchestrator that took
// over from one that ran out of quota: what happened and where things stand.
func (a *App) orchestratorHandoff(from, to string, e Exhaustion, recent []string, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[saddle] You are the orchestrator, taking over from the previous session on %s, which ran out of quota (until %s). "+
		"Its conversation can't move to %s, so this is where things stand. Carry on from here: call status for detail, and tell the owner you took over.\n",
		from, e.when(), to)
	if h, err := a.Headline(now); err == nil && h.Text != "" {
		fmt.Fprintf(&b, "\nState: %s\n", h.Text)
	}
	if ts, err := a.Store.Tasks(); err == nil {
		var live []string
		for _, t := range ts {
			if t.Role != store.RoleWorker || !t.Active() {
				continue
			}
			live = append(live, fmt.Sprintf("- %s %q: %s on %s", t.ID, t.Title, t.Status, a.taskAdapter(t).Name()))
		}
		if len(live) > 0 {
			b.WriteString("\nTasks:\n" + strings.Join(live, "\n") + "\n")
		}
	}
	if items, err := a.CatchUp(now.Add(-2*time.Hour), now); err == nil && len(items) > 0 {
		b.WriteString("\nLast two hours:\n")
		for _, it := range items {
			b.WriteString("- " + it + "\n")
		}
	}
	if len(recent) > handoffRecent {
		recent = recent[len(recent)-handoffRecent:]
	}
	if len(recent) > 0 {
		b.WriteString("\nThe end of its conversation:\n")
		for _, r := range recent {
			b.WriteString("> " + strings.ReplaceAll(strings.TrimSpace(r), "\n", " ") + "\n")
		}
	}
	return strings.TrimSpace(b.String())
}
