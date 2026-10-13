package app

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

func TestWorkerBriefRequiresRegressionTests(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	brief := a.workerBrief(store.Task{ID: "t1", Title: "x"}, nil)
	for _, want := range []string{"write a failing test", "New behavior ships with tests", "done summary"} {
		if !strings.Contains(brief, want) {
			t.Errorf("worker brief missing %q", want)
		}
	}
}

func TestOrchestratorBriefUrgentAndGettingUnstuck(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	a, _ := setup(t)
	if !strings.Contains(a.orchestratorBrief(), "which tests must exist") {
		t.Error("orchestrator brief should tell spawn prompts to name required tests")
	}
}

// #146, #152: the orchestrator brief names the queue and automerge tools.
func TestOrchestratorBriefQueueAndAutomergeTools(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	a, _ := setup(t)
	brief := a.orchestratorBrief()
	for _, want := range []string{"`saddle autopilot status`", "Never end your turn to wait", "Never ask the owner for permission to continue", "escalate only when nothing else is possible"} {
		if !strings.Contains(brief, want) {
			t.Errorf("orchestrator brief lacks %q", want)
		}
	}
}

// #257: with [claude.advisor] off, neither orchestrator brief mentions the
// advisor, and turning it on only inserts the advisor section: removing that
// section gives back the off brief byte for byte.
func TestOrchestratorBriefAdvisorOffGolden(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	for name, brief := range map[string]func() string{"tui": a.orchestratorBrief, "plugin": a.PluginBrief} {
		a.Cfg.Claude.Advisor.Enabled = false
		off := brief()
		if strings.Contains(strings.ToLower(off), "advisor") {
			t.Errorf("%s brief mentions the advisor while it is off", name)
		}
		a.Cfg.Claude.Advisor.Enabled = true
		on := brief()
		sec := a.advisorBrief()
		if sec == "" || !strings.Contains(on, sec) {
			t.Fatalf("%s brief lacks the advisor section", name)
		}
		if got := strings.Replace(on, sec, "", 1); got != off {
			t.Errorf("%s brief: advisor on changed more than its section", name)
		}
	}
}

// #257: with the advisor on, the brief names the three checkpoints, keeps the
// Haiku swarm read-only with structured summaries, and keeps Opus silent on
// routine bash and digest traffic.
func TestOrchestratorBriefAdvisorOn(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	a.Cfg.Claude.Advisor.Enabled = true
	for name, brief := range map[string]string{"tui": a.orchestratorBrief(), "plugin": a.PluginBrief()} {
		for _, want := range []string{
			"Before a plan locks", "When the same failure repeats", "failed twice", "Before calling done",
			"structured summaries only", "never edit", "claimed",
			"silent", "routine bash", "landed / merged / CI-green", "digest",
			"haiku", "opus", "3 ", "never the synchronizer",
		} {
			if !strings.Contains(brief, want) {
				t.Errorf("%s brief missing %q", name, want)
			}
		}
	}
}

// #402: both orchestrator briefs end with the same plain-language talking
// rules: headline first, no identifiers in it, translate done summaries, say
// what's next and whether anything waits on the owner. The TUI folds details;
// the plugin keeps them to a short paragraph.
func TestOrchestratorBriefTalkingGolden(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	rules := `- Write for someone who glances at the screen every hour and doesn't remember task numbers.
- Open every message with ONE plain-language headline sentence: what happened, whether it matters to the owner, and what happens next. No test names, code identifiers, SHAs or file paths in it; task and PR numbers are fine. Example: "The risk-tier work is half in: two PRs merged, four follow-ups queued as issues."
- Name things by what they do ("the auto-merge risk gate"), not by symbol. Put a task id after its name when it helps, e.g. "the meter worker (t3)".
- Never paste a worker's done summary verbatim. Translate it into what changed for the owner.
- Say what you will do next, and whether anything is waiting on the owner. "Nothing needs you." is a valid, useful sentence.
`
	want := map[string]string{
		"tui": "## Talking\n- Be brief. The sidebar is narrow.\n" + rules +
			"- Put details (test names, files, reasoning) after a blank line. The TUI folds everything after the headline paragraph; the owner opens it with a key when they want it.\n" +
			"- When a message needs the user, start it with \"‼ \" followed by ONE sentence naming the task and what is needed, then details. The TUI renders that sentence red and the rest white.\n",
		"plugin": "## Talking\n- Be brief.\n" + rules +
			"- Details (test names, files, reasoning) go in one short paragraph after a blank line, or leave them out.\n" +
			"- When a message needs the user, start it with \"‼ \" followed by ONE sentence naming the task and what is needed, then details.\n",
	}
	for name, brief := range map[string]string{"tui": a.orchestratorBrief(), "plugin": a.PluginBrief()} {
		i := strings.Index(brief, "## Talking\n")
		if i < 0 {
			t.Fatalf("%s brief has no Talking section", name)
		}
		if got := brief[i:]; got != want[name] {
			t.Errorf("%s Talking section =\n%s\nwant\n%s", name, got, want[name])
		}
	}
}

func TestOrchestratorBriefsPointAtGlossary(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	for name, brief := range map[string]string{"tui": a.orchestratorBrief(), "plugin": a.PluginBrief()} {
		if !strings.Contains(brief, "docs/GLOSSARY.md") {
			t.Errorf("%s orchestrator brief does not point at docs/GLOSSARY.md", name)
		}
	}
}
