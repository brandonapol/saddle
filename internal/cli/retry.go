package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/brandonapol/saddle/internal/app"
)

// retryCmd is `saddle retry <task>` (#936): a task saddle gave up fixing
// goes back to pr_open, its PR unlabeled needs-human and its fix count
// reset.
func retryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "retry <task>",
		Short: "Let saddle fix a task's PR again: remove needs-human and reset its fix count",
		Args:  cobra.ExactArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			if err := a.Retry(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s's fix count is reset and its PR is back in the merge queue\n", args[0])
			return nil
		}),
	}
}
