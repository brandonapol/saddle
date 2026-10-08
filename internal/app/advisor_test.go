package app

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/agent"
)

// orchArgs returns the headless orchestrator's claude args with the run dir
// and brief replaced by placeholders, plus its extra environment.
func orchArgs(t *testing.T, a *App) ([]string, []string) {
	t.Helper()
	l, _, err := a.Orchestrator()
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := l.Headless("")
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	for _, s := range cmd.Args[1:] {
		switch {
		case s == l.Brief:
			s = "<brief>"
		case strings.HasPrefix(s, l.RunDir):
			s = "<run>" + strings.TrimPrefix(s, l.RunDir)
		}
		args = append(args, s)
	}
	return args, cmd.Env
}

// stubProbe replaces the claude flag probe so tests need no binary.
func stubProbe(t *testing.T, reject string) *[]string {
	t.Helper()
	var seen []string
	prev := probeFlag
	probeFlag = func(cmd, flag, value string) error {
		seen = append(seen, flag+" "+value)
		if flag == reject {
			return errors.New("error: unknown option '" + flag + "'")
		}
		return nil
	}
	t.Cleanup(func() { probeFlag = prev })
	return &seen
}

// #257: with [claude.advisor] off, the orchestrator launches exactly as before.
func TestOrchestratorArgsAdvisorOffGolden(t *testing.T) {
	a, _ := setup(t)
	seen := stubProbe(t, "")
	args, env := orchArgs(t, a)
	want := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json",
		"--verbose", "--include-partial-messages",
		"--settings", "<run>/settings.json", "--mcp-config", "<run>/mcp.json",
		"--append-system-prompt", "<brief>",
		"--model", "sonnet", "--permission-mode", "auto",
		"--disallowedTools", strings.Join(agent.OrchestratorDeny(), ",")}
	if !slices.Equal(args, want) {
		t.Fatalf("args changed with the advisor off:\n got %q\nwant %q", args, want)
	}
	if slices.Contains(env, agent.SubagentModelEnv+"=haiku") {
		t.Fatalf("advisor off but launch sets %s", agent.SubagentModelEnv)
	}
	if len(*seen) > 0 {
		t.Fatalf("advisor off but probed claude: %q", *seen)
	}
}

// #257: with the advisor on, the lead model runs at high effort with the
// advisor and subagent models pinned by alias, and Agent is allowed so the
// subagent swarm can run.
func TestOrchestratorArgsAdvisorOn(t *testing.T) {
	a, _ := setup(t)
	seen := stubProbe(t, "")
	a.Cfg.Claude.Advisor.Enabled = true
	args, env := orchArgs(t, a)
	joined := strings.Join(args, " ")
	for _, want := range []string{"--model sonnet", "--effort high", "--advisor opus"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args lack %q: %q", want, args)
		}
	}
	if !slices.Contains(env, agent.SubagentModelEnv+"=haiku") {
		t.Errorf("env lacks %s=haiku", agent.SubagentModelEnv)
	}
	deny := args[slices.Index(args, "--disallowedTools")+1]
	if slices.Contains(strings.Split(deny, ","), "Agent") {
		t.Errorf("advisor on but Agent is still denied: %s", deny)
	}
	for _, still := range []string{"Edit", "Write", "Bash(git push:*)"} {
		if !slices.Contains(strings.Split(deny, ","), still) {
			t.Errorf("advisor on dropped deny %s", still)
		}
	}
	if !slices.Contains(*seen, "--advisor opus") || !slices.Contains(*seen, "--effort high") {
		t.Errorf("flags not probed: %q", *seen)
	}

	// Each role stays overridable.
	a.Cfg.Claude.Advisor.Lead, a.Cfg.Claude.Advisor.Advisor, a.Cfg.Claude.Advisor.Subagents = "opus", "sonnet", "sonnet"
	args, env = orchArgs(t, a)
	joined = strings.Join(args, " ")
	if !strings.Contains(joined, "--model opus") || !strings.Contains(joined, "--advisor sonnet") ||
		!slices.Contains(env, agent.SubagentModelEnv+"=sonnet") {
		t.Errorf("overrides ignored: %q", args)
	}
}

// #257: a flag the installed claude rejects fails the launch, naming the flag,
// instead of quietly running a single model.
func TestOrchestratorAdvisorRejectedFlagFails(t *testing.T) {
	a, _ := setup(t)
	stubProbe(t, "--advisor")
	a.Cfg.Claude.Advisor.Enabled = true
	_, _, err := a.Orchestrator()
	if err == nil || !strings.Contains(err.Error(), "--advisor") {
		t.Fatalf("err = %v, want one naming --advisor", err)
	}
}
