package app

import (
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/autopilot"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/usage"
)

// routingRig is three providers with quota under the balanced policy:
// claude has 40% of its budget left, grok 70% and codex 90%. gemini and
// opencode can't run here.
func routingRig(t *testing.T) *App {
	t.Helper()
	a, _ := setup(t)
	if a.Cfg.AdapterPolicy.Pick != config.PickBalanced {
		t.Fatalf("default policy = %s, want balanced", a.Cfg.AdapterPolicy.Pick)
	}
	_, err := a.SetConcurrency(16)
	must(t, err)
	a.Cfg.Adapters = config.AdapterMap{"claude": {Budget: 1000}, "grok": {Budget: 1000}, "codex": {Budget: 1000}}
	now := time.Now()
	b := func(adapter string, in int64) usage.Bucket {
		return usage.Bucket{Key: usage.Key{Minute: now.Add(-time.Hour).UTC().Truncate(time.Minute), Task: "t-" + adapter, Adapter: adapter, Model: "m-" + adapter},
			Tokens: usage.Tokens{Input: in}}
	}
	must(t, a.Store.AddUsage("s1", false, []usage.Bucket{b("claude", 600), b("grok", 300), b("codex", 100)}))
	a.AdapterStatus = func() []agent.Status {
		return []agent.Status{{Name: "claude", OK: true}, {Name: "grok", OK: true}, {Name: "codex", OK: true},
			{Name: "gemini", Reason: "not installed"}, {Name: "opencode", Reason: "not installed"}}
	}
	return a
}

// #505: a mixed backlog of hard, standard and mechanical tickets spreads
// across claude, codex and grok: hard work goes to claude (the only adapter
// suited to it by default) on opus, and the rest by remaining quota per
// running worker. Status explains each pick.
func TestBalancedSpreadsMixedBacklog(t *testing.T) {
	a := routingRig(t)
	spawn := func(title, difficulty, claim string) (string, string) {
		t.Helper()
		tk, err := a.Spawn(SpawnReq{Title: title, Difficulty: difficulty, Claims: []string{claim}})
		must(t, err)
		return a.AdapterOf(tk), tk.Model
	}
	type want struct{ title, difficulty, claim, adapter string }
	for _, w := range []want{
		{"design the thing", "hard", "a/**", "claude"},    // claude 0.4 -> 0.2 per worker
		{"rename", "mechanical", "b/**", "codex"},         // codex 0.9 -> 0.45
		{"small feature", "standard", "c/**", "grok"},     // grok 0.7 -> 0.35
		{"another rename", "mechanical", "d/**", "codex"}, // codex 0.45 -> 0.3
		{"another feature", "standard", "e/**", "grok"},   // grok 0.35
	} {
		got, model := spawn(w.title, w.difficulty, w.claim)
		if got != w.adapter {
			t.Fatalf("%s (%s) went to %s, want %s", w.title, w.difficulty, got, w.adapter)
		}
		if w.adapter == "claude" && model != "opus" {
			t.Fatalf("hard work on claude runs %q, want opus", model)
		}
	}
	picks := a.TaskPicks()
	if len(picks) != 5 {
		t.Fatalf("picks = %v", picks)
	}
	if p := picks["t1"]; !strings.HasPrefix(p, "claude (opus): ") || !strings.Contains(p, "suits hard") || !strings.Contains(p, "40% of budget left") {
		t.Errorf("t1 pick = %q", p)
	}
	if p := picks["t2"]; !strings.HasPrefix(p, "codex") || !strings.Contains(p, "suits mechanical") || !strings.Contains(p, "90% of budget left, 0 running") {
		t.Errorf("t2 pick = %q", p)
	}
}

// #505: an out-of-quota or unavailable provider is skipped, and hard work
// goes elsewhere when claude is out rather than waiting.
func TestBalancedSkipsOutAndUnavailable(t *testing.T) {
	a := routingRig(t)
	must(t, a.MarkExhausted("codex", time.Now().Add(time.Hour), ""))
	tk, err := a.Spawn(SpawnReq{Title: "rename", Difficulty: "mechanical"})
	must(t, err)
	if got := a.AdapterOf(tk); got != "grok" {
		t.Fatalf("with codex out, went to %s, want grok", got)
	}
	a.AdapterStatus = func() []agent.Status {
		return []agent.Status{{Name: "claude", OK: true}, {Name: "grok", Reason: "not logged in"}, {Name: "codex", OK: true}}
	}
	tk, err = a.Spawn(SpawnReq{Title: "another rename", Difficulty: "mechanical"})
	must(t, err)
	if got := a.AdapterOf(tk); got != "claude" {
		t.Fatalf("with codex out and grok unavailable, went to %s, want claude", got)
	}
	must(t, a.MarkExhausted("claude", time.Now().Add(time.Hour), ""))
	a.AdapterStatus = func() []agent.Status {
		return []agent.Status{{Name: "claude", OK: true}, {Name: "grok", OK: true}, {Name: "codex", OK: true}}
	}
	tk, err = a.Spawn(SpawnReq{Title: "design", Difficulty: "hard"})
	must(t, err)
	if got := a.AdapterOf(tk); got != "grok" {
		t.Fatalf("hard work with claude and codex out went to %s, want grok", got)
	}
	if p := a.TaskPicks()[tk.ID]; !strings.Contains(p, "not suited to hard") {
		t.Errorf("pick = %q", p)
	}
}

// #505: Needs routes to an adapter that declares the capability, adapter=
// overrides the chooser, and a crowded task with claims prefers hooks.
func TestNeedsAndOverride(t *testing.T) {
	a := routingRig(t)
	a.Cfg.Adapters["grok"] = config.Adapter{Budget: 1000, Capabilities: []string{"image-gen"}}
	tk, err := a.Spawn(SpawnReq{Title: "draw the logo", Needs: []string{"Image-Gen"}})
	must(t, err)
	if got := a.AdapterOf(tk); got != "grok" {
		t.Fatalf("image-gen went to %s, want grok", got)
	}
	tk, err = a.Spawn(SpawnReq{Title: "pinned", Difficulty: "mechanical", Adapter: "claude"})
	must(t, err)
	if got := a.AdapterOf(tk); got != "claude" || !strings.Contains(a.TaskPicks()[tk.ID], "requested") {
		t.Fatalf("adapter= override went to %s (%q)", got, a.TaskPicks()[tk.ID])
	}
	// Claims next to another live task's need hooks: codex has none.
	_, err = a.Spawn(SpawnReq{Title: "billing a", Adapter: "grok", Claims: []string{"billing/meter.go"}})
	must(t, err)
	tk, err = a.Spawn(SpawnReq{Title: "billing b", Difficulty: "mechanical", Claims: []string{"billing/invoice.go"}})
	must(t, err)
	if got := a.AdapterOf(tk); got == "codex" {
		t.Fatalf("crowded claims went to hookless codex: %q", a.TaskPicks()[tk.ID])
	}
	if p := a.TaskPicks()[tk.ID]; !strings.Contains(p, "hooks enforce its claims") {
		t.Errorf("pick = %q", p)
	}
	// Uncrowded claims don't need hooks under balanced.
	tk, err = a.Spawn(SpawnReq{Title: "docs", Difficulty: "mechanical", Claims: []string{"docs/**"}})
	must(t, err)
	if got := a.AdapterOf(tk); got != "codex" {
		t.Fatalf("uncrowded claims went to %s, want codex (most quota)", got)
	}
}

// #505: autopilot uses the same chooser: its plan names the provider per
// ticket, its spawns spread by difficulty and quota, and an old ticket's
// "Model scope: Opus" is a hint (the chooser still picks), not a pin.
func TestAutopilotUsesChooser(t *testing.T) {
	a := routingRig(t)
	d := a.NewAutopilot(readyQueue(
		apIssue(1, "**Difficulty: hard (design judgment).**\nclaims: alpha/**"),
		apIssue(2, "**Model scope: Opus.** an old ticket\nclaims: beta/**"),
		apIssue(3, "**Difficulty: mechanical.**\nclaims: gamma/**"),
		apIssue(4, "**Difficulty: standard.**\nProvider: claude\nclaims: delta/**")))
	_, err := d.Enable(autopilot.Options{})
	must(t, err)
	rep, err := d.Tick()
	must(t, err)
	if len(rep.Spawned) != 4 {
		t.Fatalf("spawned %+v", rep.Spawned)
	}
	ts, err := a.Store.Tasks()
	must(t, err)
	on := map[int]string{}
	model := map[int]string{}
	for _, tk := range ts {
		on[tk.Issue], model[tk.Issue] = a.AdapterOf(tk), tk.Model
	}
	if on[1] != "claude" || model[1] != "opus" {
		t.Errorf("hard ticket on %s:%s, want claude:opus", on[1], model[1])
	}
	if on[2] != "codex" || model[2] == "opus" {
		t.Errorf("old Model scope: Opus ticket on %s:%s, want codex (the hint doesn't pin claude)", on[2], model[2])
	}
	if on[3] != "grok" {
		t.Errorf("mechanical ticket on %s, want grok", on[3])
	}
	if on[4] != "claude" {
		t.Errorf("Provider: claude ticket on %s", on[4])
	}
	for _, sp := range rep.Spawned {
		if !strings.HasPrefix(sp.Route, on[sp.Issue]) || rep.Routes[sp.Issue] != sp.Route {
			t.Errorf("#%d route %q (plan %q) but ran on %s", sp.Issue, sp.Route, rep.Routes[sp.Issue], on[sp.Issue])
		}
	}
}

// #505: an explicit model= is the orchestrator's choice and prefers its
// provider; a ticket's model is only a hint the chooser never follows.
func TestExplicitModelSteersHintDoesNot(t *testing.T) {
	a := routingRig(t)
	tk, err := a.Spawn(SpawnReq{Title: "explicit", Model: "opus"})
	must(t, err)
	if got := a.AdapterOf(tk); got != "claude" || tk.Model != "opus" {
		t.Fatalf("model=opus ran %s:%s, want claude:opus", got, tk.Model)
	}
	tk, err = a.Spawn(SpawnReq{Title: "hinted", ModelHint: "opus"})
	must(t, err)
	if got := a.AdapterOf(tk); got != "codex" || tk.Model == "opus" {
		t.Fatalf("hint opus ran %s:%s, want codex on its own model", got, tk.Model)
	}
}
