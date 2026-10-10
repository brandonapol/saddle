package cli

import (
	"fmt"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/spf13/cobra"
)

func restackCmd() *cobra.Command {
	var abort, resume bool
	cmd := &cobra.Command{
		Use:   "restack",
		Short: "Rebuild landed stacks, or recover an interrupted local ref transaction",
		Args:  cobra.NoArgs,
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			if abort || resume {
				if err := a.RecoverRestack(abort); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "restack recovered; run saddle prs to publish")
				return nil
			}
			res, err := a.Restack()
			for _, move := range res.Moves {
				fmt.Fprintf(cmd.OutOrStdout(), "%s: %s → %s\n", move.Ref, short(move.Old), short(move.New))
			}
			return err
		}),
	}
	cmd.Flags().BoolVar(&abort, "abort", false, "restore the journaled old tips and train state")
	cmd.Flags().BoolVar(&resume, "continue", false, "complete the journaled new tips and train state")
	cmd.MarkFlagsMutuallyExclusive("abort", "continue")
	return cmd
}
