package app

import (
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/sso"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

// #798: a worker at a login screen marks its adapter unauthenticated, with
// no timer, and every running task on it is handed off, not just the one
// whose screen showed it. New spawns route past it until it is cleared.
func TestAuthHitMarksAdapterAndHandsOffItsTasks(t *testing.T) {
	a, ft, sink := rotationApp(t)
	a.Cfg.AdapterOrder = []string{"claude", "codex", "grok"}
	a.Cfg.AdapterPolicy = config.AdapterPolicy{Pick: config.PickOrder, Failover: config.FailoverHandoff, HandoffWait: time.Hour}
	one, err := a.Spawn(SpawnReq{Title: "one", Claims: []string{"one/**"}})
	must(t, err)
	two, err := a.Spawn(SpawnReq{Title: "two", Claims: []string{"two/**"}})
	must(t, err)
	_ = orchNotices(t, a, "")

	must(t, a.AuthHit(one.ID))
	e, out := a.ExhaustedAdapters(time.Now())[usage.Claude]
	if !out || e.Reason != ReasonLogin || !e.Until.IsZero() {
		t.Fatalf("claude after a login screen: %+v out=%v, want out for login with no Until", e, out)
	}
	if _, still := a.ExhaustedAdapters(time.Now().Add(30 * 24 * time.Hour))[usage.Claude]; !still {
		t.Fatal("unauthenticated expired on a timer")
	}
	for _, tk := range []store.Task{one, two} {
		got, err := a.Store.Task(tk.ID)
		must(t, err)
		if a.AdapterOf(got) == usage.Claude || got.Window == tk.Window || ft.Alive(tk.Window) {
			t.Fatalf("%s not handed off: adapter %s window %s", tk.ID, a.AdapterOf(got), got.Window)
		}
	}
	if got := sink.all(); len(got) != 1 || !strings.Contains(got[0], "login") {
		t.Fatalf("owner notifications = %q, want one about the login", got)
	}
	if n := strings.Join(orchNotices(t, a, ""), "\n"); !strings.Contains(n, agent.LoginHint(usage.Claude)) {
		t.Fatalf("orchestrator not told how to log in: %q", n)
	}

	// A second login screen on the same adapter says nothing new.
	must(t, a.MarkUnauthenticated(usage.Claude, "another login screen"))
	if got := sink.all(); len(got) != 1 {
		t.Fatalf("owner paged again: %q", got)
	}

	next, err := a.Spawn(SpawnReq{Title: "after"})
	must(t, err)
	if r := agent.Recorded(a.stateDir("run", next.ID)); r == usage.Claude {
		t.Fatal("new spawn routed to the logged-out adapter")
	}

	must(t, a.ClearUnauthenticated(usage.Claude))
	if _, out := a.ExhaustedAdapters(time.Now())[usage.Claude]; out {
		t.Fatal("still out after a successful login")
	}
}

// #798: clearing a login leaves an adapter out of quota alone.
func TestClearUnauthenticatedKeepsQuotaExhaustion(t *testing.T) {
	a, _, _ := rotationApp(t)
	must(t, a.MarkExhausted(usage.Grok, time.Now().Add(time.Hour), ""))
	must(t, a.ClearUnauthenticated(usage.Grok))
	if _, out := a.ExhaustedAdapters(time.Now())[usage.Grok]; !out {
		t.Fatal("clearing a login restored an adapter out of quota")
	}
}

// #798: an expired codex SSO login hands off the codex agents already
// running, not only new spawns, and pages the owner once (with the sign-in
// code, not a second "logged out" page). A finished sign-in puts codex back.
func TestSSOLoginNeededHandsOffRunningCodexAgents(t *testing.T) {
	a, ft, sink := rotationApp(t)
	a.Cfg.AdapterPolicy = config.AdapterPolicy{Failover: config.FailoverHandoff}
	cx, err := a.Spawn(SpawnReq{Title: "codex work", Adapter: usage.Codex, Force: true})
	must(t, err)

	must(t, a.SSOLoginNeeded("corp", sso.DeviceAuth{UserCode: "ABCD-EFGH", VerificationURI: "https://idp/device", ExpiresAt: time.Now().Add(10 * time.Minute)}))
	if e, out := a.ExhaustedAdapters(time.Now())[usage.Codex]; !out || e.Reason != ReasonLogin {
		t.Fatalf("codex not marked logged out: %+v %v", e, out)
	}
	got, err := a.Store.Task(cx.ID)
	must(t, err)
	if a.AdapterOf(got) == usage.Codex || ft.Alive(cx.Window) {
		t.Fatalf("running codex agent not handed off: %s", a.AdapterOf(got))
	}
	if pages := sink.all(); len(pages) != 1 || !strings.Contains(pages[0], "ABCD-EFGH") {
		t.Fatalf("owner pages = %q, want one with the sign-in code", pages)
	}

	must(t, a.SSOLoggedIn("corp"))
	if _, out := a.ExhaustedAdapters(time.Now())[usage.Codex]; out {
		t.Fatal("codex still out after the SSO sign-in finished")
	}
}
