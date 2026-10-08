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

// #176: the brief states the live cap and names the concurrency tool.
func TestOrchestratorBriefConcurrency(t *testing.T) {
	a, _ := setup(t)
	_, err := a.SetConcurrency(3)
	must(t, err)
	brief := a.orchestratorBrief()
	for _, want := range []string{"Run at most 3 at once", "`concurrency`"} {
		if !strings.Contains(brief, want) {
			t.Errorf("orchestrator brief lacks %q", want)
		}
	}
}

// #212: the brief names the repo's gate and forbids --no-verify.
func TestWorkerBriefNamesRepoCheck(t *testing.T) {
	a, _ := setup(t)
	brief := a.workerBrief(store.Task{ID: "t1", Title: "x"}, nil)
	if !strings.Contains(brief, "--no-verify") {
		t.Error("brief lacks the --no-verify rule")
	}
	write(t, a.Root, "Makefile", "check:\n\ttrue\nfix:\n\ttrue\n")
	brief = a.workerBrief(store.Task{ID: "t1", Title: "x"}, nil)
	for _, want := range []string{"`make check`", "`make fix`", "--no-verify", "done runs it too"} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief lacks %q", want)
		}
	}
}

// #220/#213: the orchestrator learns publish is the way out of a blocked
// prs, never a hand push, and that ci-red holds the layers above.
func TestOrchestratorBriefPublishAndCIRed(t *testing.T) {
	a, _ := setup(t)
	b := a.orchestratorBrief()
	for _, want := range []string{"saddle publish <task>", "Never `git push` or `gh pr create` by hand", "auto-mode classifier", "holds the layers above it"} {
		if !strings.Contains(b, want) {
			t.Errorf("orchestrator brief lacks %q", want)
		}
	}
}

// #256: the orchestrator brief says what to do while autopilot is on.
func TestOrchestratorBriefAutopilot(t *testing.T) {
	a, _ := setup(t)
	brief := a.orchestratorBrief()
	for _, want := range []string{"`saddle autopilot status`", "Never end your turn to wait", "Never ask the owner for permission to continue", "escalate only when nothing else is possible"} {
		if !strings.Contains(brief, want) {
			t.Errorf("orchestrator brief lacks %q", want)
		}
	}
}
