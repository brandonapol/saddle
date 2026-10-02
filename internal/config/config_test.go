package config

import (
	"os"
	"path/filepath"
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
	if cfg.CI != (CI{Interval: 10 * time.Minute}) {
		t.Fatalf("ci defaults: %+v", cfg.CI)
	}
}

func TestCIConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".saddle"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[ci]\ninterval = \"3m\"\ndisabled = true\n"
	if err := os.WriteFile(filepath.Join(root, ".saddle", "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CI != (CI{Interval: 3 * time.Minute, Disabled: true}) {
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
