package app

import (
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/usage"
)

func picked(rs []AdapterChoice) string {
	var out []string
	for _, r := range rs {
		if r.Rank > 0 {
			out = append(out, r.Name)
		}
	}
	return strings.Join(out, ",")
}

// #481: policy = "order" ranks the session default first and the rest in
// [adapters] order; a kind listed in [adapters.<name>] kinds wins over it;
// a task with claims prefers hooked fallbacks, but never over the default.
func TestRankAdaptersByOrderKindAndClaims(t *testing.T) {
	a, _ := setup(t)
	a.Cfg.AdapterOrder = []string{"claude", "codex", "grok"}
	now := time.Now()
	if got := picked(a.rankAdapters(pickReq{def: "claude"}, nil, now)); got != "claude,codex,grok" {
		t.Fatalf("order = %s", got)
	}
	// Rotation starts at the default, as before #481.
	if got := picked(a.rankAdapters(pickReq{def: "codex"}, nil, now)); got != "codex,grok,claude" {
		t.Fatalf("order from codex = %s", got)
	}
	a.Cfg.Adapters = config.AdapterMap{"codex": {Kinds: []string{"docs"}}}
	if got := picked(a.rankAdapters(pickReq{def: "claude", kind: "docs"}, nil, now)); got != "codex,claude,grok" {
		t.Fatalf("kind docs = %s", got)
	}
	// claude out of quota: a task with claims skips codex (no hooks) for grok.
	ex := map[string]Exhaustion{"claude": {Adapter: "claude", Until: now.Add(time.Hour)}}
	rs := a.rankAdapters(pickReq{def: "claude", claims: true}, ex, now)
	if got := picked(rs); got != "grok,codex" {
		t.Fatalf("claims with claude out = %s (%+v)", got, rs)
	}
	if !strings.Contains(rs[0].Why, "hooks") || !strings.Contains(rs[len(rs)-1].Why, "out of quota") {
		t.Fatalf("why = %+v", rs)
	}
	// saddle up codex keeps codex for a task with claims.
	if got := picked(a.rankAdapters(pickReq{def: "codex", claims: true}, nil, now)); !strings.HasPrefix(got, "codex,") {
		t.Fatalf("claims under codex default = %s", got)
	}
}

// #481: policy = "cost" picks the cheapest adapter; "quota" the one with the
// most of its five-hour budget left, an adapter without a budget counting as
// unlimited. Ones that can't run here are never picked.
func TestRankAdaptersByCostAndQuota(t *testing.T) {
	a, _ := setup(t)
	now := time.Now()
	a.Cfg.AdapterOrder = []string{"claude", "grok", "codex"}
	a.Cfg.AdapterPolicy.Pick = config.PickCost
	a.Cfg.Adapters = config.AdapterMap{"claude": {Cost: 3}, "grok": {Cost: 2}, "codex": {Cost: 0.5}}
	if got := picked(a.rankAdapters(pickReq{def: "claude"}, nil, now)); got != "codex,grok,claude" {
		t.Fatalf("cost = %s", got)
	}

	a.Cfg.AdapterPolicy.Pick = config.PickQuota
	a.Cfg.Adapters = config.AdapterMap{"claude": {Budget: 1000}, "grok": {Budget: 1000}, "codex": {Budget: 1000}}
	b := func(adapter string, in int64) usage.Bucket {
		return usage.Bucket{Key: usage.Key{Minute: now.Add(-time.Hour).UTC().Truncate(time.Minute), Task: "t-" + adapter, Adapter: adapter, Model: "m-" + adapter},
			Tokens: usage.Tokens{Input: in, CacheRead: 5000}}
	}
	must(t, a.Store.AddUsage("s1", false, []usage.Bucket{b("claude", 900), b("grok", 200), b("codex", 600)}))
	rs := a.rankAdapters(pickReq{def: "claude"}, nil, now)
	if got := picked(rs); got != "grok,codex,claude" {
		t.Fatalf("quota = %s (%+v)", got, rs)
	}
	if rs[0].Why != "80% of budget left" {
		t.Fatalf("why = %q", rs[0].Why)
	}

	a.AdapterStatus = func() []agent.Status {
		return []agent.Status{{Name: "claude", OK: true}, {Name: "grok", OK: false, Reason: "not installed"}, {Name: "codex", OK: true}}
	}
	if got := picked(a.rankAdapters(pickReq{def: "claude"}, nil, now)); got != "codex,claude" {
		t.Fatalf("quota without grok = %s", got)
	}
}

// #481: a spawn's kind picks the adapter configured for it, and the spawn
// event says why.
func TestSpawnKindPicksAdapter(t *testing.T) {
	a, _ := setup(t)
	a.Cfg.Adapters = config.AdapterMap{"grok": {Kinds: []string{"docs"}}}
	tk, err := a.Spawn(SpawnReq{Title: "readme", Kind: "Docs"})
	must(t, err)
	if got := agent.Recorded(a.stateDir("run", tk.ID)); got != usage.Grok {
		t.Fatalf("docs spawn ran %s, want grok", got)
	}
	plain, err := a.Spawn(SpawnReq{Title: "code"})
	must(t, err)
	if got := agent.Recorded(a.stateDir("run", plain.ID)); got != usage.Claude {
		t.Fatalf("plain spawn ran %s, want claude", got)
	}
	evs, err := a.Store.Events(100)
	must(t, err)
	found := false
	for _, e := range evs {
		found = found || (e.Task == tk.ID && e.Kind == "spawn" && strings.Contains(e.Data, "adapter=grok (kind docs"))
	}
	if !found {
		t.Fatalf("no spawn event naming why: %+v", evs)
	}
}

// #520: the default rotation picks opencode while it has quota and skips it
// once it is out. An explicit spawn is refused the same way; a [fallback]
// chain can name it, and is skipped once it is out too.
func TestPickSkipsOpenCodeWhenOutOfQuota(t *testing.T) {
	a, _ := setup(t)
	now := time.Now()
	if got := picked(a.rankAdapters(pickReq{def: a.Cfg.Harness}, nil, now)); !strings.Contains(","+got+",", ",opencode,") {
		t.Fatalf("pick left opencode out: %s", got)
	}
	var listed agent.Status
	for _, s := range a.Adapters() {
		if s.Name == agent.OpenCodeName {
			listed = s
		}
	}
	if listed.Provider != "any (opencode)" || listed.Hooks || listed.PickRank == 0 || listed.OutOfQuotaUntil != "" {
		t.Fatalf("status opencode = %+v", listed)
	}
	ex := map[string]Exhaustion{"opencode": {Adapter: "opencode", Until: now.Add(time.Hour), Resets: "in 2 hours"}}
	rs := a.rankAdapters(pickReq{def: a.Cfg.Harness}, ex, now)
	if strings.Contains(","+picked(rs)+",", ",opencode,") {
		t.Fatalf("picked opencode while it is out: %+v", rs)
	}
	var why string
	for _, r := range rs {
		if r.Name == "opencode" {
			why = r.Why
			if r.Rank != 0 {
				t.Fatalf("out opencode rank %d, want 0 (%+v)", r.Rank, r)
			}
		}
	}
	if !strings.Contains(why, "out of quota") {
		t.Fatalf("opencode why = %q", why)
	}

	w, err := a.Spawn(SpawnReq{Title: "on opencode", Adapter: agent.OpenCodeName})
	must(t, err)
	if got := agent.Recorded(a.stateDir("run", w.ID)); got != agent.OpenCodeName {
		t.Fatalf("spawn ran %s, want opencode", got)
	}
	must(t, a.MarkExhausted(agent.OpenCodeName, now.Add(time.Hour), "in 2 hours"))
	if _, err := a.Spawn(SpawnReq{Title: "opencode again", Adapter: agent.OpenCodeName}); err == nil || !strings.Contains(err.Error(), "out of quota") {
		t.Fatalf("explicit opencode while out: %v", err)
	}

	a.Cfg.Fallback = map[string][]string{"claude": {"opencode"}}
	must(t, a.MarkExhausted(usage.Claude, now.Add(time.Hour), "3pm"))
	// opencode is out, so the chain is skipped and the policy's next adapter runs.
	next, err := a.Spawn(SpawnReq{Title: "claude out, opencode out"})
	must(t, err)
	if got := agent.Recorded(a.stateDir("run", next.ID)); got == agent.OpenCodeName || got == usage.Claude {
		t.Fatalf("spawn ran %s, want it to skip claude and opencode", got)
	}
}
