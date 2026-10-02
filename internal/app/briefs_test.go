package app

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

func TestWorkerBriefRequiresRegressionTests(t *testing.T) {
	a, _ := setup(t)
	brief := a.workerBrief(store.Task{ID: "t1", Title: "x"}, nil)
	for _, want := range []string{"write a failing test", "New behavior ships with tests", "done summary"} {
		if !strings.Contains(brief, want) {
			t.Errorf("worker brief missing %q", want)
		}
	}
}

func TestOrchestratorBriefUrgentAndGettingUnstuck(t *testing.T) {
	a, _ := setup(t)
	brief := a.orchestratorBrief()
	for _, want := range []string{
		"‼ ", "ONE sentence",
		"hand-fix", "back it up first", "failing-first", "GitHub issue",
		"`unstack`", "`sentinel_ack`", "`requeue`",
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("orchestrator brief missing %q", want)
		}
	}
	if strings.Contains(brief, "never spawn a worker") {
		t.Error("brief still forbids repair workers")
	}
}

func TestOrchestratorBriefAsksForTests(t *testing.T) {
	a, _ := setup(t)
	if !strings.Contains(a.orchestratorBrief(), "which tests must exist") {
		t.Error("orchestrator brief should tell spawn prompts to name required tests")
	}
}
