package cli

import (
	"fmt"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/spf13/cobra"
)

func gcCmd() *cobra.Command {
	var dry bool
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Remove worktrees, branches and refs left by killed, failed or landed tasks",
		Long: `Lists worktrees, saddle/* task branches and refs/saddle/* refs that no live
task uses, then removes them. Branches with commits not on the integration
branch and worktrees with uncommitted changes are listed but kept.`,
		Args: cobra.NoArgs,
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			list, run := a.Leftovers, a.GC
			if dry {
				run = list
			}
			ls, err := run()
			out := cmd.OutOrStdout()
			for _, l := range ls {
				switch {
				case l.Keep != "":
					fmt.Fprintf(out, "kept     %-8s %s (%s)\n", l.Kind, l.Name, l.Keep)
				case dry:
					fmt.Fprintf(out, "leftover %-8s %s\n", l.Kind, l.Name)
				default:
					fmt.Fprintf(out, "removed  %-8s %s\n", l.Kind, l.Name)
				}
			}
			if len(ls) == 0 && err == nil {
				fmt.Fprintln(out, "nothing to clean up")
			}
			return err
		}),
	}
	cmd.Flags().BoolVarP(&dry, "dry-run", "n", false, "only list what would be removed")
	return cmd
}
