package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/brandonapol/saddle/internal/doctor"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/runq"
	"github.com/spf13/cobra"
)

func doctorCmd() *cobra.Command {
	var asJSON, fix, trusted bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the repo, gh, tools and hooks saddle needs, with a fix for each problem",
		Long: `Runs saddle's preflight checks: git remote and default branch, gh login and
scopes, repo merge settings, branch protection, test.cmd, tmux and claude, the
ref guard hooks, .saddle/ being ignored, state.db, stale worktrees, and the
skills and slash commands the orchestrator sees (from a short claude session
that makes no model call).

Each check is ok, warn or fail, and the output groups what isn't ok: what
saddle fixed, what --fix can fix locally (it runs saddle init, keeping
config.toml), what you need to do, and what is optional. Exits non-zero when
any check fails; warnings alone exit zero.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Doctor must work before saddle init, so it doesn't open the app
			// (which would create .saddle/).
			root := os.Getenv("SADDLE_ROOT")
			if root == "" {
				wd, err := os.Getwd()
				if err != nil {
					return err
				}
				if root, err = gitx.Root(wd); err != nil {
					return fmt.Errorf("not in a git repo: %w", err)
				}
			}
			env := doctor.WithSkillsProbe(root)
			var rs []doctor.Result
			if fix {
				// --fix writes hooks and config, so it asks for trust first (#215).
				if err := gateTrust(cmd.OutOrStdout(), cmd.InOrStdin(), trusted); err != nil {
					return err
				}
				var err error
				rs, err = doctor.Fix(env, func() error {
					a, err := open()
					if err != nil {
						return err
					}
					defer a.Close()
					return a.Init()
				})
				if err != nil {
					return err
				}
			} else {
				rs = doctor.Run(env)
			}
			rs = append(rs, shimCheck(filepath.Join(root, ".saddle", "shims"), os.Getenv), versionCheck(root, buildInfo()))
			return reportDoctor(cmd.OutOrStdout(), rs, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().BoolVar(&fix, "fix", false, "apply every local fix saddle can make safely (runs saddle init; keeps config.toml)")
	cmd.Flags().BoolVar(&trusted, "trust", false, "with --fix, trust this repo without asking (remembered; see saddle trust)")
	return cmd
}

// reportDoctor prints the results and returns an error when a check failed,
// so the command exits non-zero.
func reportDoctor(out io.Writer, rs []doctor.Result, asJSON bool) error {
	if asJSON {
		if err := doctor.WriteJSON(out, rs); err != nil {
			return err
		}
	} else {
		doctor.WriteTable(out, rs)
	}
	if doctor.Failed(rs) {
		return errors.New("saddle doctor found failing checks")
	}
	return nil
}

// shimCheck (#240) checks the heavy-run shims agents get on PATH: each finds
// its real tool, and none is shadowed. Inside an agent pane (SADDLE_TASK
// set) it checks the pane's own PATH; elsewhere the PATH a pane would get.
func shimCheck(dir string, getenv func(string) string) doctor.Result {
	const name = "runq shims"
	if _, err := os.Stat(filepath.Join(dir, runq.ShimMarker)); err != nil {
		return doctor.Result{Name: name, Status: doctor.OK, Detail: "none yet; saddle writes them when it spawns an agent"}
	}
	path := getenv("PATH")
	if getenv("SADDLE_TASK") == "" {
		path = dir + string(os.PathListSeparator) + path
	}
	if ps := runq.CheckShims(dir, path); len(ps) > 0 {
		return doctor.Result{Name: name, Status: doctor.Warn, Detail: strings.Join(ps, "; "),
			Fix: "agents' panes need " + dir + " first on PATH; respawn the agent, or set SADDLE_RUNQ=off to skip the queue"}
	}
	return doctor.Result{Name: name, Status: doctor.OK, Detail: dir + " resolves every shimmed tool"}
}
