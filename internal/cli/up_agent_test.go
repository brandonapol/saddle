package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/agent/agenttest"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/doctor"
	"github.com/brandonapol/saddle/internal/mcpserver"
)

// #150: saddle up [agent] [epic-file|-].
func TestParseUpArgs(t *testing.T) {
	dir := t.TempDir()
	epic := filepath.Join(dir, "epic.md")
	if err := os.WriteFile(epic, []byte("do it"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	// A file literally named after an agent: the bare word is the agent.
	if err := os.WriteFile("grok", []byte("epic"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args          []string
		harness, epic string
	}{
		{nil, "", ""},
		{[]string{"claude"}, "claude", ""},
		{[]string{"grok"}, "grok", ""},
		{[]string{"codex"}, "codex", ""},
		{[]string{"grok", epic}, "grok", epic},
		{[]string{"claude", "-"}, "claude", "-"},
		{[]string{"-"}, "", "-"},
		{[]string{epic}, "", epic},
		{[]string{"./grok"}, "", "./grok"},
		{[]string{"claude", "./grok"}, "claude", "./grok"},
	} {
		h, e, err := parseUpArgs(c.args)
		if err != nil || h != c.harness || e != c.epic {
			t.Errorf("parseUpArgs(%q) = %q, %q, %v; want %q, %q", c.args, h, e, err, c.harness, c.epic)
		}
	}
	for _, bad := range [][]string{{"cursor"}, {epic, "-"}, {"grok", "codex", "-"}} {
		if _, _, err := parseUpArgs(bad); err == nil {
			t.Errorf("parseUpArgs(%q) should fail", bad)
		} else if bad[0] == "cursor" && !strings.Contains(err.Error(), "claude, codex, grok") {
			t.Errorf("unknown word should list the agents: %v", err)
		}
	}
}

// The chosen agent applies to this process tree only: it is set in the
// environment config.Load reads, and the config file is untouched.
func TestUpAgentSetsHarnessEnv(t *testing.T) {
	t.Setenv("SADDLE_HARNESS", "")
	if err := applyUpHarness("codex"); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("SADDLE_HARNESS"); got != "codex" {
		t.Errorf("SADDLE_HARNESS = %q", got)
	}
	t.Setenv("SADDLE_HARNESS", "grok")
	if err := applyUpHarness(""); err != nil || os.Getenv("SADDLE_HARNESS") != "grok" {
		t.Errorf("bare up must leave the inherited harness alone: %q %v", os.Getenv("SADDLE_HARNESS"), err)
	}
}

func TestWriteAdapters(t *testing.T) {
	var b strings.Builder
	writeAdapters(&b, []agent.Status{{Name: "claude", OK: true}, {Name: "gemini", Reason: "gemini not on PATH"}, {Name: "grok", OK: true}})
	if got := b.String(); got != "adapters: claude, grok; gemini unavailable (gemini not on PATH)\n" {
		t.Errorf("got %q", got)
	}
}

// #481: status says which adapter the next spawn takes, in order and why,
// and the policy behind it; ones that can't be picked are left out.
func TestWriteAdapterPicks(t *testing.T) {
	var b strings.Builder
	writeAdapterPicks(&b, []agent.Status{
		{Name: "claude", OK: true, OutOfQuotaUntil: "3pm", Pick: "out of quota until 3pm"},
		{Name: "codex", OK: true, PickRank: 2, Pick: "next in order"},
		{Name: "grok", OK: true, PickRank: 1, Pick: "next in order"},
	}, "policy order, failover handoff")
	if got := b.String(); got != "next spawn: grok (next in order), codex (next in order); policy order, failover handoff\n" {
		t.Fatalf("got %q", got)
	}
}

// #482: status shows each adapter's excluded models as such.
func TestWriteAdaptersShowsExcludedModels(t *testing.T) {
	var b strings.Builder
	writeAdapters(&b, []agent.Status{{Name: "claude", OK: true}, {Name: "codex", OK: true, ExcludedModels: []string{"astra"}}})
	if got := b.String(); got != "adapters: claude, codex; codex excluded models: astra\n" {
		t.Errorf("got %q", got)
	}
}

// #505: status names, for each live task, the adapter the chooser put it
// on and why; finished tasks are left out.
func TestWriteTaskPicks(t *testing.T) {
	var b strings.Builder
	writeTaskPicks(&b, []mcpserver.TaskView{
		{ID: "t1", Status: "running", Pick: "codex: suits mechanical, 90% of budget left, 0 running"},
		{ID: "t2", Status: "landed", Pick: "claude (opus): suits hard"},
		{ID: "t3", Status: "running"},
	})
	if got := b.String(); got != "t1 on codex: suits mechanical, 90% of budget left, 0 running\n" {
		t.Fatalf("got %q", got)
	}
}

// #646: a bare saddle up whose harness can't run falls back to the first
// agent CLI that can, for this run, and with none it prints the install
// steps instead of starting.
func TestUpFallback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bins   []string
		logins []string // nil: every bin is logged in
		want   string
		says   []string
		err    string
	}{
		{"claude ready", []string{"claude", "codex"}, nil, "", nil, ""},
		{"claude missing, codex present", []string{"codex"}, nil, "codex", []string{"claude can't run (claude not on PATH); running codex for this run", "Codex has no hooks"}, ""},
		{"claude missing, grok present", []string{"grok", "codex"}, nil, "grok", []string{"running grok for this run", `harness = "grok"`}, ""},
		// No credentials file is only a guess at logged out (apiKeyHelper
		// and the like leave none), so an installed claude still starts.
		{"claude looks logged out", []string{"claude", "codex"}, []string{"codex"}, "", []string{"claude is not logged in: run claude /login; starting it anyway"}, ""},
		{"claude looks logged out, nothing else", []string{"claude"}, []string{}, "", []string{"starting it anyway"}, ""},
		// A codex-only VM whose codex login isn't in ~/.codex/auth.json
		// (its keyring store, say) still runs codex instead of failing.
		{"claude missing, codex looks logged out", []string{"codex"}, []string{}, "codex",
			[]string{"codex is not logged in: run codex login; starting it anyway", "running codex for this run"}, ""},
		{"nothing", nil, nil, "", nil, "no agent CLI is installed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logins := tc.logins
			if logins == nil {
				logins = tc.bins
			}
			agenttest.Path(t, tc.bins, logins)
			cfg := config.Default()
			var out strings.Builder
			got, err := upFallback(&out, cfg, agent.Availability(doctor.AgentCmds(cfg)))
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) || !strings.Contains(err.Error(), "@anthropic-ai/claude-code") {
					t.Fatalf("err = %v, want %q with install steps", err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("upFallback = %q, %v; want %q", got, err, tc.want)
			}
			for _, w := range tc.says {
				if !strings.Contains(out.String(), w) {
					t.Errorf("output lacks %q:\n%s", w, out.String())
				}
			}
		})
	}
}
