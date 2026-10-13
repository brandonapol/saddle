package app

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/usage"
)

// #481: the orchestrator's adapter runs out: claude is marked out, the
// orchestrator moves to grok (it has a headless session; codex has none),
// and the new session opens with a state summary naming the live tasks and
// the end of the old conversation. Its brief, status identity and transcript
// follow it. Once claude has quota again a fresh start goes back to claude.
func TestFailOverOrchestrator(t *testing.T) {
	a, _, _ := rotationApp(t)
	a.Cfg.AdapterOrder = []string{"claude", "codex", "grok"}
	tk, err := a.Spawn(SpawnReq{Title: "meter work"})
	must(t, err)
	if _, _, err := a.Orchestrator(); err != nil {
		t.Fatal(err)
	}
	must(t, a.Store.SetField(OrchestratorID, "session_id", "claude-session"))

	l, msg, err := a.FailOverOrchestrator(usage.LimitBanner{Resets: "in 3 hours"}, []string{"user: land t1 when it is green", "assistant: will do"})
	must(t, err)
	if l.Kind != agent.KindGrok || a.orchestratorAgent() != usage.Grok || a.OrchestratorIdentity().Adapter != usage.Grok {
		t.Fatalf("launch kind %v, orchestrator agent %s", l.Kind, a.orchestratorAgent())
	}
	if !strings.Contains(l.Brief, "the Grok CLI") {
		t.Error("the new session's brief doesn't name grok")
	}
	if _, out := a.ExhaustedAdapters(time.Now())[usage.Claude]; !out {
		t.Fatal("claude not marked out of quota")
	}
	for _, want := range []string{"taking over from the previous session on claude", tk.ID, "meter work", "land t1 when it is green"} {
		if !strings.Contains(msg, want) {
			t.Errorf("handoff lacks %q:\n%s", want, msg)
		}
	}
	if o, _ := a.Store.Task(OrchestratorID); o.SessionID != "" {
		t.Fatalf("kept claude's session %q for grok", o.SessionID)
	}

	// Grok runs out too: nowhere to go.
	if _, _, err := a.FailOverOrchestrator(usage.LimitBanner{Resets: "in 2 hours"}, nil); !errors.Is(err, ErrNoOrchestratorAdapter) {
		t.Fatalf("both out: err = %v", err)
	}

	must(t, a.ClearExhausted(usage.Claude))
	l, _, err = a.Orchestrator()
	must(t, err)
	if l.Kind != agent.KindClaude || a.orchestratorAgent() != usage.Claude {
		t.Fatalf("after claude's reset the orchestrator runs %s", a.orchestratorAgent())
	}
}

// #481: quota errors from either CLI read as the orchestrator running out;
// other API errors don't.
func TestOrchestratorOutOfQuota(t *testing.T) {
	for _, s := range []string{
		"Claude AI usage limit reached|1760000000",
		"API Error: You've hit your session limit · resets 3pm (America/New_York)",
		"Error: 429 You exceeded your current quota, please check your plan and billing details.",
		"Your team has reached its monthly spending limit.",
	} {
		if _, ok := OrchestratorOutOfQuota(s); !ok {
			t.Errorf("%q not read as out of quota", s)
		}
	}
	for _, s := range []string{"API Error: 529 Overloaded", "the turn failed and gave no reason", ""} {
		if _, ok := OrchestratorOutOfQuota(s); ok {
			t.Errorf("%q read as out of quota", s)
		}
	}
}
