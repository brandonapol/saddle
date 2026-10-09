package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/scratch"
	"github.com/spf13/cobra"
)

func gcCmd() *cobra.Command {
	var dry bool
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Remove worktrees, branches and refs left by killed, failed or landed tasks",
		Long: `Fetches, then lists worktrees, saddle/* task branches (local and on the
remote) and refs/saddle/* refs that no live task uses, and removes them. A
branch counts as merged when it is on the integration branch or base, when its
commits are on base under other SHAs (squash or rebase merges, matched by tree,
patch-id or file content), or when its task's PR merged into base at its tip.
Branches with unmerged work, the remote branch of a PR still in the stack, the
checked-out branch and worktrees with uncommitted changes are listed with the
reason and kept. It also sweeps saddle's scratch (docs/scratch.md): entries
under the scratch root and saddle's go-build*, e2ebin* and saddle-* entries
in the OS temp dir, older than [tmp] max_age (default 60m) and held by no
live process. Nothing else in /tmp is touched. --dry-run lists without
fetching or removing.`,
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
			res := a.SweepScratch(dry, 0)
			writeScratchSweep(out, res, dry)
			if len(ls) == 0 && len(res.Removed) == 0 && err == nil {
				fmt.Fprintln(out, "nothing to clean up")
			}
			return errors.Join(append([]error{err}, res.Errs...)...)
		}),
	}
	cmd.Flags().BoolVarP(&dry, "dry-run", "n", false, "only list what would be removed")
	return cmd
}

// writeScratchSweep lists what the scratch sweep removed (or would), and
// what it kept because a live process holds it.
func writeScratchSweep(out io.Writer, res scratch.Result, dry bool) {
	verb := "removed "
	if dry {
		verb = "leftover"
	}
	for _, e := range res.Removed {
		fmt.Fprintf(out, "%s %-8s %s (%s)\n", verb, "scratch", e.Path, scratch.Bytes(e.Bytes))
	}
	for _, e := range res.Kept {
		if !strings.HasPrefix(e.Reason, "fresh") {
			fmt.Fprintf(out, "kept     %-8s %s (%s)\n", "scratch", e.Path, e.Reason)
		}
	}
	if len(res.Removed) > 0 {
		what := "freed"
		if dry {
			what = "would free"
		}
		fmt.Fprintf(out, "scratch: %s %s\n", what, scratch.Bytes(res.Freed))
	}
}
