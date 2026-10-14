package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/sre"
)

func openIncident(t *testing.T, a *app.App, investigators ...string) {
	t.Helper()
	inc := &sre.Incident{ID: "api-500s", Opened: time.Now(), Investigators: investigators,
		ReadOnly: [][]string{{"psql", "-c", "select count(*) from users"}}}
	if err := sre.OpenIncident(a.Root, inc); err != nil {
		t.Fatal(err)
	}
}

func bash(t *testing.T, a *app.App, task, cmd string) (decision, reason string) {
	t.Helper()
	out := run(t, a, task, map[string]any{
		"hook_event_name": "PreToolUse", "tool_name": "Bash",
		"tool_input": map[string]any{"command": cmd},
	})
	hs, _ := out["hookSpecificOutput"].(map[string]any)
	d, _ := hs["permissionDecision"].(string)
	r, _ := hs["permissionDecisionReason"].(string)
	return d, r
}

func sreCards(t *testing.T, a *app.App) []app.DecisionCard {
	t.Helper()
	cs, err := a.Decisions()
	if err != nil {
		t.Fatal(err)
	}
	var out []app.DecisionCard
	for _, c := range cs {
		if c.Class == SREDecisionClass {
			out = append(out, c)
		}
	}
	return out
}

func update(t *testing.T, a *app.App, fn func(*sre.Incident) error) error {
	t.Helper()
	return sre.UpdateIncident(a.Root, fn)
}

const rollback = "kubectl rollout undo deploy/api"

func TestSREGuardDeniesMutatingBeforeVerification(t *testing.T) {
	a := setup(t)
	openIncident(t, a, "t1", "t2")
	if d, _ := bash(t, a, "t1", "kubectl get pods -n prod"); d == "deny" {
		t.Fatal("read-only command denied")
	}
	if d, _ := bash(t, a, "t1", "psql -c 'select count(*) from users'"); d == "deny" {
		t.Fatal("an sre.toml check command denied")
	}
	d, r := bash(t, a, "t1", rollback)
	if d != "deny" || !strings.Contains(r, RuleSREGuard) || !strings.Contains(r, "saddle sre") {
		t.Fatalf("mutating before any root cause: %q %q", d, r)
	}
	// A stated but unverified root cause still locks.
	if err := update(t, a, func(inc *sre.Incident) error {
		_, err := inc.State("t1", "bad deploy 4f2a", "errors start at the deploy", time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if d, _ := bash(t, a, "t1", rollback); d != "deny" {
		t.Fatalf("mutating with an unverified root cause: %q", d)
	}
	if n := len(sreCards(t, a)); n != 0 {
		t.Fatalf("filed %d decisions while locked", n)
	}
	// A task outside the incident is not guarded.
	if d, _ := bash(t, a, "t9", rollback); d == "deny" {
		t.Fatal("task outside the incident denied")
	}
}

func TestSREGuardUnknownCommandIsMutating(t *testing.T) {
	a := setup(t)
	openIncident(t, a, "t1")
	for _, cmd := range []string{"some-ops-tool --fix", "psql -c 'delete from users'"} {
		if d, _ := bash(t, a, "t1", cmd); d != "deny" {
			t.Fatalf("%q: decision %q, want deny", cmd, d)
		}
	}
	out := run(t, a, "t1", map[string]any{
		"hook_event_name": "PreToolUse", "tool_name": "mcp__pagerduty__resolve_incident",
		"tool_input": map[string]any{"id": "P123"},
	})
	hs, _ := out["hookSpecificOutput"].(map[string]any)
	if hs["permissionDecision"] != "deny" {
		t.Fatalf("unknown MCP tool: %v", out)
	}
}

func TestSREGuardDeniesWhenVerifierIsProposer(t *testing.T) {
	a := setup(t)
	openIncident(t, a, "t1", "t2")
	var id int
	if err := update(t, a, func(inc *sre.Incident) error {
		var err error
		id, err = inc.State("t1", "bad deploy 4f2a", "errors start at the deploy", time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := update(t, a, func(inc *sre.Incident) error { return inc.Verify(id, "t1", "I looked again", time.Now()) }); err == nil {
		t.Fatal("self-verification accepted")
	}
	if d, _ := bash(t, a, "t1", rollback); d != "deny" {
		t.Fatalf("mutating after self-verification: %q", d)
	}
	if n := len(sreCards(t, a)); n != 0 {
		t.Fatalf("filed %d decisions after self-verification", n)
	}
}

func TestSREGuardFilesIrreversibleDecisionAfterDistinctVerification(t *testing.T) {
	a := setup(t)
	openIncident(t, a, "t1", "t2")
	if err := update(t, a, func(inc *sre.Incident) error {
		id, err := inc.State("t1", "bad deploy 4f2a", "errors start at the deploy", time.Now())
		if err != nil {
			return err
		}
		return inc.Verify(id, "t2", "canary on 4f29 is clean", time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	// Still not run: proposed to the owner instead.
	d, r := bash(t, a, "t2", rollback)
	if d != "deny" || !strings.Contains(r, "decision queue") {
		t.Fatalf("mutating after verification: %q %q", d, r)
	}
	cs := sreCards(t, a)
	if len(cs) != 1 {
		t.Fatalf("decisions = %+v, want one", cs)
	}
	c := cs[0]
	if !c.Irreversible || !c.Open() || c.Subject != rollback || c.Task != "t2" ||
		c.Options[sreRunOption-1].Action != rollback || c.Options[sreRunOption-1].Recommended {
		t.Fatalf("card = %+v", c)
	}
	if !strings.Contains(c.What, "bad deploy 4f2a") || !strings.Contains(c.What, "t1") || !strings.Contains(c.What, "t2") {
		t.Fatalf("card does not name the root cause and both agents: %q", c.What)
	}
	// Retrying files nothing new and is still denied.
	if d, _ := bash(t, a, "t2", rollback); d != "deny" {
		t.Fatalf("retry before the owner answered: %q", d)
	}
	if n := len(sreCards(t, a)); n != 1 {
		t.Fatalf("retry filed again: %d cards", n)
	}
	inc, _ := sre.LoadIncident(a.Root)
	if len(inc.Proposals) != 1 {
		t.Fatalf("proposals = %+v", inc.Proposals)
	}
	// The owner picks "run it": now, and only that command, may run.
	if _, err := a.AnswerDecisionID(c.ID, sreRunOption, time.Now()); err != nil {
		t.Fatal(err)
	}
	if d, r := bash(t, a, "t2", rollback); d == "deny" {
		t.Fatalf("approved action denied: %q", r)
	}
	if d, _ := bash(t, a, "t2", "kubectl delete pod api-1"); d != "deny" {
		t.Fatal("approval of one action let another through")
	}
	if d, _ := bash(t, a, "t1", rollback); d != "deny" {
		t.Fatal("approval for t2 let t1 run it")
	}
}

func TestSREGuardHeldDecisionStaysDenied(t *testing.T) {
	a := setup(t)
	openIncident(t, a, "t1", "t2")
	if err := update(t, a, func(inc *sre.Incident) error {
		id, _ := inc.State("t1", "bad deploy", "e", time.Now())
		return inc.Verify(id, "t2", "confirmed", time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	bash(t, a, "t1", rollback)
	c := sreCards(t, a)[0]
	if _, err := a.AnswerDecisionID(c.ID, sreHoldOption, time.Now()); err != nil {
		t.Fatal(err)
	}
	if d, _ := bash(t, a, "t1", rollback); d != "deny" {
		t.Fatal("held action ran")
	}
}

func TestSREGuardClosedIncidentLiftsIt(t *testing.T) {
	a := setup(t)
	openIncident(t, a, "t1")
	if err := update(t, a, func(inc *sre.Incident) error { inc.Close(time.Now()); return nil }); err != nil {
		t.Fatal(err)
	}
	if d, _ := bash(t, a, "t1", rollback); d == "deny" {
		t.Fatal("closed incident still guards")
	}
}

func TestSREGuardCorruptStateFailsClosedWithEscape(t *testing.T) {
	a := setup(t)
	p := filepath.Join(a.Root, sre.IncidentFile)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, r := bash(t, a, "t1", rollback)
	if d != "deny" || !strings.Contains(r, sre.IncidentFile) {
		t.Fatalf("corrupt incident: %q %q", d, r)
	}
	// Read-only commands keep working, so investigation never stalls.
	if d, _ := bash(t, a, "t1", "kubectl get pods"); d == "deny" {
		t.Fatal("corrupt incident blocked a read-only command")
	}
}
