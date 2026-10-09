// Package initcmd runs saddle init's local setup on the first saddle up and
// says what it did (#163): config.toml, test.cmd, the ref guard hooks and the
// .git/info/exclude entry. Init is idempotent, so running it again repairs a
// half-set-up repo without touching an existing config.toml. Callers must
// only run it in a repo the user trusts (#215).
package initcmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/refguard"
)

// State is which init steps are in place in a repo.
type State struct {
	Config  bool   // .saddle/config.toml exists
	TestCmd string // the configured test.cmd, "" when none
	Hooks   bool   // both ref guard hooks are saddle's and their binary exists
	Ignored bool   // git ignores .saddle/
}

// Complete reports whether every step init always does is in place. A
// missing test.cmd doesn't count: init only sets it when it detects one.
func (s State) Complete() bool { return s.Config && s.Hooks && s.Ignored }

// Inspect reads the repo whose main checkout is root.
func Inspect(root string) State {
	var s State
	if _, err := os.Stat(filepath.Join(root, ".saddle", "config.toml")); err == nil {
		s.Config = true
	}
	if cfg, err := config.Load(root); err == nil {
		s.TestCmd = cfg.Test.Cmd
	}
	if hs, err := refguard.Installed(root); err == nil && len(hs) > 0 {
		s.Hooks = true
		for _, h := range hs {
			if _, err := os.Stat(h.Bin); !h.Present || !h.Saddle || err != nil {
				s.Hooks = false
			}
		}
	}
	_, err := gitx.Run(root, "check-ignore", "-q", ".saddle/")
	s.Ignored = err == nil
	return s
}

// Ensure runs init (App.Init) and returns, in plain words, the steps it
// completed that weren't in place before: nothing when the repo was
// already set up.
func Ensure(root string, init func() error) ([]string, error) {
	before := Inspect(root)
	if err := init(); err != nil {
		return nil, fmt.Errorf("saddle init: %w", err)
	}
	after := Inspect(root)
	var steps []string
	if !before.Config && after.Config {
		steps = append(steps, "wrote .saddle/config.toml")
	}
	if before.TestCmd == "" && after.TestCmd != "" {
		steps = append(steps, "detected test.cmd "+after.TestCmd)
	}
	if !before.Hooks && after.Hooks {
		steps = append(steps, "installed the git hooks that stop agents from moving saddle's own branches")
	}
	if !before.Ignored && after.Ignored {
		steps = append(steps, "added /.saddle/ to .git/info/exclude")
	}
	return steps, nil
}

// Report prints one line naming the steps Ensure completed, and nothing
// when there were none.
func Report(w io.Writer, steps []string) {
	if len(steps) == 0 {
		return
	}
	fmt.Fprintf(w, "saddle set up this repo: %s\n", strings.Join(steps, "; "))
}
