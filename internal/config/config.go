// Package config loads saddle's TOML config: defaults, then
// ~/.config/saddle/config.toml, then <repo>/.saddle/config.toml.
package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	// Base is the branch the epic starts from and the bottom of the PR stack.
	Base string `toml:"base"`
	// Integration is the branch the merge train lands onto, one task at a time.
	Integration string `toml:"integration"`
	// Session is the tmux session name. Empty means "saddle-<repo dir name>".
	Session string `toml:"session"`
	// Concurrency caps how many worker agents run at once.
	Concurrency int `toml:"concurrency"`
	// Serial globs are files only the merge train should change (lockfiles, migrations).
	Serial []string `toml:"serial"`
	// CloseOnLand kills a task's tmux window and removes its worktree once it lands.
	CloseOnLand bool `toml:"close_on_land"`

	Test   Test   `toml:"test"`
	Claude Claude `toml:"claude"`
	Triage Triage `toml:"triage"`
	Usage  Usage  `toml:"usage"`
}

// Usage configures token metering and the plan-limit bars.
type Usage struct {
	// Poll is how often transcripts are read for new usage.
	Poll time.Duration `toml:"poll"`
	// CountCacheReads includes cache-read tokens in window totals. Off by
	// default: they dwarf everything else and weigh little against plan limits.
	CountCacheReads bool `toml:"count_cache_reads"`
	// Windows are trailing windows shown as bars. Setting any replaces the defaults.
	Windows []Window `toml:"windows"`
}

// Window is a trailing usage window, e.g. the last 5 hours.
type Window struct {
	Name string        `toml:"name"`
	Span time.Duration `toml:"span"`
	// Cap is the token cap for the window; 0 means no cap, so no bar, only a total.
	Cap int64 `toml:"cap"`
}

// Triage gates attention with TypeSafe's Jev when TYPESAFE_API_KEY is set.
type Triage struct {
	// Disabled turns Jev triage off even when a key is present.
	Disabled bool `toml:"disabled"`
	// NoAutoApprove keeps routine prompts going to the orchestrator instead of
	// being answered "Yes" automatically.
	NoAutoApprove bool `toml:"no_auto_approve"`
}

type Test struct {
	// Cmd runs in the task worktree after rebasing onto integration; non-zero blocks landing.
	Cmd string `toml:"cmd"`
}

type Claude struct {
	Cmd               string `toml:"cmd"`
	Model             string `toml:"model"`
	OrchestratorModel string `toml:"orchestrator_model"`
	PermissionMode    string `toml:"permission_mode"`
}

func Default() Config {
	return Config{
		Base:        "main",
		Integration: "saddle/integration",
		Concurrency: 5,
		CloseOnLand: true,
		Claude: Claude{
			Cmd:               "claude",
			Model:             "opus",
			OrchestratorModel: "sonnet",
			PermissionMode:    "auto",
		},
		Usage: Usage{
			Poll: 15 * time.Second,
			Windows: []Window{
				{Name: "5h", Span: 5 * time.Hour},
				{Name: "7d", Span: 7 * 24 * time.Hour},
			},
		},
	}
}

// Load merges config files over the defaults. Missing files are skipped.
func Load(root string) (Config, error) {
	cfg := Default()
	paths := []string{filepath.Join(root, ".saddle", "config.toml")}
	if home, err := os.UserConfigDir(); err == nil {
		paths = append([]string{filepath.Join(home, "saddle", "config.toml")}, paths...)
	}
	for _, p := range paths {
		// TOML decodes arrays element by element over what is already there, so
		// a file that sets windows would otherwise inherit leftover defaults.
		prev := cfg.Usage.Windows
		cfg.Usage.Windows = nil
		md, err := toml.DecodeFile(p, &cfg)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return cfg, err
		}
		if !md.IsDefined("usage", "windows") {
			cfg.Usage.Windows = prev
		}
	}
	if cfg.Session == "" {
		cfg.Session = "saddle-" + filepath.Base(root)
	}
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	if cfg.Usage.Poll <= 0 {
		cfg.Usage.Poll = Default().Usage.Poll
	}
	var ws []Window
	for _, w := range cfg.Usage.Windows {
		if w.Span > 0 {
			ws = append(ws, w)
		}
	}
	cfg.Usage.Windows = ws
	return cfg, nil
}

const Template = `# saddle per-repo config. See docs/ARCHITECTURE.md.
# base = "main"
# integration = "saddle/integration"
# concurrency = 5
# close_on_land = true
# serial = ["go.sum", "db/migrations/**"]

[test]
# cmd = "go test ./..."

[triage]
# Uses TypeSafe Jev (set JEV_TOKEN; make setup asks for it) to decide which agent events reach
# the orchestrator or you, and to auto-approve routine permission prompts.
# disabled = false
# no_auto_approve = false

[claude]
# model = "opus"                 # workers
# orchestrator_model = "sonnet"  # the chat agent in the TUI
# permission_mode = "auto"

[usage]
# Plan-limit bars are estimates: set cap to your plan's token budget for each
# window. A window with no cap shows only its total.
# poll = "15s"
# count_cache_reads = false
# [[usage.windows]]
# name = "5h"
# span = "5h"
# cap = 0
# [[usage.windows]]
# name = "7d"
# span = "168h"
# cap = 0
`
