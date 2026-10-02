package cli

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/brandonapol/saddle/internal/doctor"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/spf13/cobra"
)

func doctorCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the repo, gh, tools and hooks saddle needs, with a fix for each problem",
		Long: `Runs saddle's preflight checks: git remote and default branch, gh login and
scopes, repo merge settings, branch protection, test.cmd, tmux and claude, the
ref guard hooks, .saddle/ being ignored, state.db, and stale worktrees.

Each check is ok, warn or fail. Exits non-zero when any check fails;
warnings alone exit zero.`,
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
			return reportDoctor(cmd.OutOrStdout(), doctor.Run(doctor.System(root)), asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
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
