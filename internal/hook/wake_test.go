package hook

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/store"
)

// #793: with no mod heartbeat and no `plugin wait` heartbeat, nothing wakes
// an idle session, so SessionStart tells it to start one wait now.
func TestOrchestratorSessionStartRearmsWait(t *testing.T) {
	a := setup(t)
	if _, err := a.EnsureOrchestrator(); err != nil {
		t.Fatal(err)
	}
	out := HandleOrchestrator(a, Input{Event: "SessionStart", Source: "compact"})
	if out == nil || !strings.Contains(out.Specific.AdditionalContext, "saddle plugin wait") {
		t.Fatalf("SessionStart without mod or wait: %+v", out)
	}
	// A wait already running (its heartbeat is fresh) needs no second one.
	if err := os.WriteFile(a.WaitHeartbeatPath(), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := HandleOrchestrator(a, Input{Event: "SessionStart", Source: "startup"}); out != nil && strings.Contains(out.Specific.AdditionalContext, "saddle plugin wait") {
		t.Fatalf("asked for a second wait: %+v", out)
	}
}

// #793: with workers live and no way left to wake it, a Stop continues once
// to start the wait.
func TestOrchestratorStopRearmsWaitWhileWorkersRun(t *testing.T) {
	a := setup(t)
	if _, err := a.EnsureOrchestrator(); err != nil {
		t.Fatal(err)
	}
	if out := HandleOrchestrator(a, Input{Event: "Stop"}); out != nil {
		t.Fatalf("no workers, yet the stop was held: %+v", out)
	}
	if err := a.Store.CreateTask(store.Task{ID: "t1", Title: "w", Status: store.Running, Role: store.RoleWorker, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	out := HandleOrchestrator(a, Input{Event: "Stop"})
	if out == nil || out.Decision != "block" || !strings.Contains(out.Reason, "saddle plugin wait") {
		t.Fatalf("Stop with a live worker and no wait: %+v", out)
	}
	if out := HandleOrchestrator(a, Input{Event: "Stop", StopHookActive: true}); out != nil {
		t.Fatalf("held a stop that already continued: %+v", out)
	}
}

// #793: a card's notice reaches t0 once; after compaction SessionStart
// still lists it, so a compacted orchestrator doesn't forget it.
func TestOrchestratorSessionStartListsOpenCards(t *testing.T) {
	a := setup(t)
	if _, err := a.EnsureOrchestrator(); err != nil {
		t.Fatal(err)
	}
	c, err := a.FileDecision(app.DecisionReq{
		By: "t5", Task: "t5", Class: "release-tag", Subject: "v1.4.0", Question: "Push the release tag v1.4.0?", Irreversible: true,
		Options: []app.DecisionOption{{Label: "Push it", Action: "git push origin v1.4.0", Recommended: true}, {Label: "Wait", Action: "nothing"}},
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.TakeNotices(app.OrchestratorID, false); err != nil {
		t.Fatal(err)
	}
	out := HandleOrchestrator(a, Input{Event: "SessionStart", Source: "compact"})
	if out == nil || !strings.Contains(out.Specific.AdditionalContext, c.ID) {
		t.Fatalf("SessionStart after compaction lacks card %s: %+v", c.ID, out)
	}
	// The mod being active doesn't hide the cards either.
	if err := os.WriteFile(a.ModHeartbeatPath(), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out = HandleOrchestrator(a, Input{Event: "SessionStart", Source: "resume"})
	if out == nil || !strings.Contains(out.Specific.AdditionalContext, c.ID) {
		t.Fatalf("SessionStart on resume with the mod active lacks card %s: %+v", c.ID, out)
	}
	if strings.Contains(out.Specific.AdditionalContext, "saddle plugin wait") {
		t.Errorf("asked for a wait while the mod is active: %q", out.Specific.AdditionalContext)
	}
}
