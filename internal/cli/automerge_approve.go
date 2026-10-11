package cli

import (
	"fmt"
	"strings"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/spf13/cobra"
)

// automergeApproveCmd lets a stack or PR past the max_tier gate (#332).
func automergeApproveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "approve <stack|task|pr>",
		Short: "Let a PR above [automerge] max_tier auto-merge, until its head changes",
		Long: `Approves a PR held for its risk tier, at its current head SHA: a stack's ID
approves every PR in it, a task, PR URL or number approves that one PR. A new
push to the PR clears the approval.`,
		Args: cobra.ExactArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			prs, err := a.NewAutomerge(nil).Approve(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "approved %s at its current head; a new push clears it\n", strings.Join(prs, ", "))
			return nil
		}),
	}
}
