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

// #146, #152: the orchestrator brief names the queue and automerge tools.
func TestOrchestratorBriefQueueAndAutomergeTools(t *testing.T) {
	a, _ := setup(t)
	brief := a.orchestratorBrief()
	for _, want := range []string{"`queue_move`", "`queue_hold`", "`queue_release`", "`automerge`", "saddle stack rebase"} {
		if !strings.Contains(brief, want) {
			t.Errorf("orchestrator brief lacks %s", want)
		}
	}
}

// #179: the orchestrator brief says when and how to compact its context.
func TestOrchestratorBriefCompaction(t *testing.T) {
	a, _ := setup(t)
	for name, brief := range map[string]string{"tui": a.orchestratorBrief(), "plugin": a.PluginBrief()} {
		for _, want := range []string{
			"Compact your context at natural breakpoints", "before a long planning step",
			"Run /compact", "running tasks, open PRs, queued follow-ups, owner decisions pending, rules in force",
			"saddle may send the command for you",
		} {
			if !strings.Contains(brief, want) {
				t.Errorf("%s brief lacks %q", name, want)
			}
		}
	}
}
