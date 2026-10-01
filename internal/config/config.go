// Package config loads saddle's TOML config: defaults, then
// ~/.config/saddle/config.toml, then <repo>/.saddle/config.toml.
package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

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
		if _, err := toml.DecodeFile(p, &cfg); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return cfg, err
		}
	}
	if cfg.Session == "" {
		cfg.Session = "saddle-" + filepath.Base(root)
	}
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
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
`
