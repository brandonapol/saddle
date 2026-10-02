package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
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
