package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/brandonapol/saddle/internal/usage"
)

func TestUsageDefaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	u := cfg.Usage
	if u.Poll != 15*time.Second || u.CountCacheReads || len(u.Windows) != 2 ||
		u.Windows[0] != (Window{Name: "5h", Span: 5 * time.Hour}) ||
		u.Windows[1] != (Window{Name: "7d", Span: 168 * time.Hour}) {
		t.Fatalf("defaults: %+v", u)
	}
}

func TestUsageCaps(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	body := `
[usage]
poll = "1m"
count_cache_reads = true
[[usage.windows]]
name = "5h"
span = "5h"
cap = 2000000
[[usage.windows]]
name = "broken"
`
	if err := os.MkdirAll(filepath.Join(root, ".saddle"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".saddle", "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	u := cfg.Usage
	if u.Poll != time.Minute || !u.CountCacheReads || len(u.Windows) != 1 ||
		u.Windows[0] != (Window{Name: "5h", Span: 5 * time.Hour, Cap: 2_000_000}) {
		t.Fatalf("usage: %+v", u)
	}
}

// The commented template stays valid TOML once uncommented.
func TestTemplateParses(t *testing.T) {
	var cfg Config
	if _, err := toml.Decode(Template, &cfg); err != nil {
		t.Fatal(err)
	}
}

func TestSweeperDefaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := Sweeper{Method: "squash", ReviewLabel: "requires review"}
	if cfg.Sweeper != want {
		t.Fatalf("sweeper defaults: got %+v, want %+v", cfg.Sweeper, want)
	}
}

func TestSweeperConfig(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       Sweeper
		wantErr    bool
	}{
		{"enabled", "[sweeper]\nenabled = true\nmethod = \"rebase\"\nreview_label = \"needs eyes\"\ndry_run = true\n",
			Sweeper{Enabled: true, Method: "rebase", ReviewLabel: "needs eyes", DryRun: true}, false},
		{"blanks fall back", "[sweeper]\nenabled = true\nmethod = \"\"\nreview_label = \" \"\n",
			Sweeper{Enabled: true, Method: "squash", ReviewLabel: "requires review"}, false},
		{"bad method", "[sweeper]\nmethod = \"octopus\"\n", Sweeper{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, ".saddle"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, ".saddle", "config.toml"), []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(root)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", cfg.Sweeper)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Sweeper != tc.want {
				t.Fatalf("got %+v, want %+v", cfg.Sweeper, tc.want)
			}
		})
	}
}

func TestCIDefaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CI != (CI{Interval: 10 * time.Minute, RedInterval: 2 * time.Minute, RedMaxInterval: 16 * time.Minute, RepairAttempts: 2}) {
		t.Fatalf("ci defaults: %+v", cfg.CI)
	}
}

func TestCIConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".saddle"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[ci]\ninterval = \"3m\"\ndisabled = true\nred_interval = \"30s\"\nred_max_interval = \"5m\"\nrepair_attempts = 3\n"
	if err := os.WriteFile(filepath.Join(root, ".saddle", "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CI != (CI{Interval: 3 * time.Minute, Disabled: true, RedInterval: 30 * time.Second, RedMaxInterval: 5 * time.Minute, RepairAttempts: 3}) {
		t.Fatalf("ci: %+v", cfg.CI)
	}
}

func TestLimitsAndNarratorDefaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := cfg.Limits
	if l.FiveHour != (usage.Cap{}) || l.Weekly != (usage.Cap{}) || l.PauseLaunches || l.WarnAt != 0 || !l.WeeklyReset.IsZero() {
		t.Fatalf("limits default: %+v", l)
	}
	// No cap means the narrator is off: it never spends without opting in.
	if cfg.Narrator.DailyCapUSD != 0 || cfg.Narrator.Model != "" {
		t.Fatalf("narrator default: %+v", cfg.Narrator)
	}
}

func TestLimitsAndNarratorParse(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	body := `
[limits]
warn_at = 0.7
pause_launches = true
weekly_reset = 2026-09-29T08:00:00Z
[limits.five_hour]
tokens = 2000000
[limits.weekly]
usd = 150.5
[limits.prices.claude-opus]
input = 9
output = 45

[narrator]
daily_cap_usd = 0.5
model = "claude-haiku-4-5"
`
	writeConfig(t, root, body)
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	l := cfg.Limits
	want := time.Date(2026, 9, 29, 8, 0, 0, 0, time.UTC)
	if l.FiveHour.Tokens != 2_000_000 || l.Weekly.USD != 150.5 || l.WarnAt != 0.7 || !l.PauseLaunches ||
		!l.WeeklyReset.Equal(want) || l.Prices["claude-opus"].Input != 9 || l.Prices["claude-opus"].Output != 45 {
		t.Fatalf("limits: %+v", l)
	}
	if cfg.Narrator.DailyCapUSD != 0.5 || cfg.Narrator.Model != "claude-haiku-4-5" {
		t.Fatalf("narrator: %+v", cfg.Narrator)
	}
}

func TestLimitsRejectBadValues(t *testing.T) {
	for _, body := range []string{
		"[limits]\nwarn_at = 1.5\n",
		"[limits.five_hour]\ntokens = -1\n",
		"[narrator]\ndaily_cap_usd = -2\n",
	} {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		root := t.TempDir()
		writeConfig(t, root, body)
		if _, err := Load(root); err == nil {
			t.Errorf("%q: want error", body)
		}
	}
}

func TestHarnessGrok(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	writeConfig(t, root, `
harness = "grok"
[grok]
model = "grok-4.5"
orchestrator_model = "grok-4.5"
`)
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Harness != HarnessGrok || cfg.Grok.Model != "grok-4.5" || cfg.Grok.Cmd != "grok" || cfg.Grok.PermissionMode != "bypassPermissions" {
		t.Fatalf("grok config: %+v harness=%s", cfg.Grok, cfg.Harness)
	}
	if cfg.Claude.Model != "opus" {
		t.Fatalf("claude defaults should stay: %+v", cfg.Claude)
	}
}

func TestHarnessUnknown(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	writeConfig(t, root, "harness = \"codex\"\n")
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "harness") {
		t.Fatalf("err = %v", err)
	}
}

func writeConfig(t *testing.T, root, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".saddle"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".saddle", "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRegenParses(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	writeConfig(t, root, `
[[regen]]
paths = ["go.sum"]
cmd = "go mod tidy"
[[regen]]
paths = ["internal/db/*.sql.go", "mocks/**"]
cmd = "make generate"
`)
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Regen) != 2 || cfg.Regen[0].Cmd != "go mod tidy" || cfg.Regen[0].Paths[0] != "go.sum" ||
		len(cfg.Regen[1].Paths) != 2 || cfg.Regen[1].Cmd != "make generate" {
		t.Fatalf("regen: %+v", cfg.Regen)
	}
}

func TestRegenRejectsIncompleteEntries(t *testing.T) {
	for _, body := range []string{
		"[[regen]]\npaths = [\"go.sum\"]\n",
		"[[regen]]\ncmd = \"go mod tidy\"\n",
	} {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		root := t.TempDir()
		writeConfig(t, root, body)
		if _, err := Load(root); err == nil {
			t.Errorf("%q: want error", body)
		}
	}
}

func TestTrainDefaultsAndParse(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Train.MaxAttempts != 2 || cfg.Train.Output != "stack" || cfg.Train.NoAutoRebase {
		t.Fatalf("train defaults: %+v", cfg.Train)
	}
	root := t.TempDir()
	writeConfig(t, root, "[train]\nmax_attempts = 4\noutput = \"single\"\nno_auto_rebase = true\n")
	if cfg, err = Load(root); err != nil || cfg.Train.MaxAttempts != 4 || cfg.Train.Output != "single" || !cfg.Train.NoAutoRebase {
		t.Fatalf("train = %+v, %v", cfg.Train, err)
	}
	writeConfig(t, root, "[train]\noutput = \"linear\"\n")
	if _, err := Load(root); err == nil {
		t.Fatal("bad train.output: want error")
	}
}

// #142: adapter commands and spawn caps come from config.
func TestAdaptersAndSpawnCaps(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Spawn.MaxDepth != 3 || cfg.Spawn.MaxChildren != 8 || len(cfg.Adapters) != 0 {
		t.Fatalf("defaults: spawn %+v, adapters %+v", cfg.Spawn, cfg.Adapters)
	}
	root := t.TempDir()
	writeConfig(t, root, "[adapters.codex]\ncmd = \"/opt/codex\"\nargs = [\"--sandbox\", \"x\"]\n[spawn]\nmax_depth = 5\nmax_children = 2\n")
	if cfg, err = Load(root); err != nil {
		t.Fatal(err)
	}
	if c := cfg.Adapters["codex"]; c.Cmd != "/opt/codex" || len(c.Args) != 2 || c.Args[1] != "x" {
		t.Fatalf("adapters.codex = %+v", c)
	}
	if cfg.Spawn.MaxDepth != 5 || cfg.Spawn.MaxChildren != 2 {
		t.Fatalf("spawn = %+v", cfg.Spawn)
	}
	writeConfig(t, root, "[spawn]\nmax_depth = -1\n")
	if _, err := Load(root); err == nil {
		t.Fatal("negative spawn.max_depth: want error")
	}
}

// #152: auto-merge is off unless the config turns it on.
func TestAutoMergeDefaultOff(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Train.AutoMerge {
		t.Fatal("train.auto_merge defaults on")
	}
	root := t.TempDir()
	writeConfig(t, root, "[train]\nauto_merge = true\n")
	if cfg, err = Load(root); err != nil || !cfg.Train.AutoMerge {
		t.Fatalf("train = %+v, %v", cfg.Train, err)
	}
}

// The template documents the new keys, uncommented they still parse.
func TestTemplateDocumentsNewKeys(t *testing.T) {
	for _, k := range []string{"auto_merge", "[adapters.codex]", "[spawn]", "max_depth", "max_children"} {
		if !strings.Contains(Template, k) {
			t.Errorf("template lacks %s", k)
		}
	}
	var lines []string
	for _, l := range strings.Split(Template, "\n") {
		if s, ok := strings.CutPrefix(l, "# "); ok && (strings.HasPrefix(s, "[adapters") || strings.HasPrefix(s, "[spawn]") ||
			strings.HasPrefix(s, "cmd = \"codex\"") || strings.HasPrefix(s, "max_")) {
			l = s
		}
		lines = append(lines, l)
	}
	var cfg Config
	if _, err := toml.Decode(strings.Join(lines, "\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Spawn.MaxDepth != 3 || cfg.Adapters["codex"].Cmd != "codex" {
		t.Fatalf("uncommented template: spawn %+v adapters %+v", cfg.Spawn, cfg.Adapters)
	}
}

// #179: orchestrator.compact_at defaults to 0.7 and must be in (0, 1].
func TestOrchestratorCompactAt(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Orchestrator.CompactAt != 0.7 {
		t.Fatalf("compact_at default = %v, want 0.7", cfg.Orchestrator.CompactAt)
	}
	root := t.TempDir()
	for body, want := range map[string]float64{"0.55": 0.55, "1": 1} {
		writeConfig(t, root, "[orchestrator]\ncompact_at = "+body+"\n")
		if cfg, err = Load(root); err != nil || cfg.Orchestrator.CompactAt != want {
			t.Fatalf("compact_at = %s: got %v, %v", body, cfg.Orchestrator.CompactAt, err)
		}
	}
	for _, bad := range []string{"0", "-0.1", "1.5"} {
		writeConfig(t, root, "[orchestrator]\ncompact_at = "+bad+"\n")
		if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "compact_at") {
			t.Errorf("compact_at = %s: err %v, want a compact_at error", bad, err)
		}
	}
	if !strings.Contains(Template, "[orchestrator]") || !strings.Contains(Template, "# compact_at = 0.7") {
		t.Error("template doesn't document orchestrator.compact_at")
	}
}

// #183: notices.wake_after defaults to 3 minutes; zero or less means the default.
func TestNoticesWakeAfter(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notices.WakeAfter != 3*time.Minute {
		t.Fatalf("wake_after default = %v, want 3m", cfg.Notices.WakeAfter)
	}
	root := t.TempDir()
	for body, want := range map[string]time.Duration{`"90s"`: 90 * time.Second, `"0s"`: 3 * time.Minute} {
		writeConfig(t, root, "[notices]\nwake_after = "+body+"\n")
		if cfg, err = Load(root); err != nil || cfg.Notices.WakeAfter != want {
			t.Fatalf("wake_after = %s: got %v, %v", body, cfg.Notices.WakeAfter, err)
		}
	}
	if !strings.Contains(Template, "[notices]") || !strings.Contains(Template, `# wake_after = "3m"`) {
		t.Error("template doesn't document notices.wake_after")
	}
}

// #211: [train] stack_backend picks how stacks are published: "saddle"
// chains PR bases, "gh-stack" also links them natively on GitHub.
func TestStackBackend(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Train.StackBackend != StackBackendSaddle {
		t.Fatalf("default stack_backend = %q, want saddle", cfg.Train.StackBackend)
	}
	root := t.TempDir()
	writeConfig(t, root, "[train]\nstack_backend = \"gh-stack\"\n")
	if cfg, err = Load(root); err != nil || cfg.Train.StackBackend != StackBackendGhStack {
		t.Fatalf("train = %+v, %v", cfg.Train, err)
	}
	writeConfig(t, root, "[train]\nstack_backend = \"graphite\"\n")
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "train.stack_backend") {
		t.Fatalf("bad stack_backend: err = %v", err)
	}
	if !strings.Contains(Template, "stack_backend") {
		t.Error("template lacks stack_backend")
	}
}

// [train] lint.cmd: unset auto-detects, "" disables, anything else runs (#212).
func TestTrainLintCmd(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, tc := range []struct {
		body     string
		set      bool
		cmd      string
		disabled bool
	}{
		{"", false, "", false},
		{"[train]\nlint.cmd = \"make lint\"\n", true, "make lint", false},
		{"[train]\nlint.cmd = \"\"\n", true, "", true},
		{"[train.lint]\ncmd = \"make check\"\n", true, "make check", false},
	} {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, ".saddle"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".saddle", "config.toml"), []byte(tc.body), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(root)
		if err != nil {
			t.Fatal(err)
		}
		l := cfg.Train.Lint
		if l.Set != tc.set || l.Cmd != tc.cmd || l.Disabled() != tc.disabled {
			t.Errorf("%q: got %+v (disabled %v)", tc.body, l, l.Disabled())
		}
	}
}

// #222: notices.digest_every defaults to 15 minutes; zero or less means the default.
func TestNoticesDigestEvery(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notices.DigestEvery != 15*time.Minute {
		t.Fatalf("digest_every default = %v, want 15m", cfg.Notices.DigestEvery)
	}
	root := t.TempDir()
	for body, want := range map[string]time.Duration{`"5m"`: 5 * time.Minute, `"0s"`: 15 * time.Minute} {
		writeConfig(t, root, "[notices]\ndigest_every = "+body+"\n")
		if cfg, err = Load(root); err != nil || cfg.Notices.DigestEvery != want {
			t.Fatalf("digest_every = %s: got %v, %v", body, cfg.Notices.DigestEvery, err)
		}
	}
	if !strings.Contains(Template, `# digest_every = "15m"`) {
		t.Error("template doesn't document notices.digest_every")
	}
}
