package cli

import (
	"fmt"
	"strings"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/spf13/cobra"
)

func noticesCmd() *cobra.Command {
	var all bool
	var n int
	cmd := &cobra.Command{
		Use:   "notices",
		Short: "Show the orchestrator's pending digest, or with --all every notice and how it was routed",
		Long: `Only questions and real decisions interrupt the orchestrator (#222). Routine
news (landed, merged, restacked, CI green) is rolled into one digest line at
most every notices.digest_every, and no-ops and repeats are silenced.

Without flags this prints the digest that would be sent now. --all lists
every orchestrator notice with its class: interrupt, digest, silent, and the
digest lines sent.`,
		Args: cobra.NoArgs,
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			out := cmd.OutOrStdout()
			if !all {
				line, err := a.PendingDigest()
				if err != nil {
					return err
				}
				if line == "" {
					line = "Nothing new since the last digest."
				}
				fmt.Fprintln(out, line)
				return nil
			}
			es, err := a.NoticeLog(n)
			if err != nil {
				return err
			}
			if len(es) == 0 {
				fmt.Fprintln(out, "No orchestrator notices yet.")
			}
			for _, e := range es {
				class := strings.TrimPrefix(e.Kind, "notice_")
				fmt.Fprintf(out, "%s %-11s %s\n", e.TS.Format("01-02 15:04"), class, strings.ReplaceAll(e.Data, "\n", " "))
			}
			return nil
		}),
	}
	cmd.Flags().BoolVar(&all, "all", false, "list every orchestrator notice, digested and silenced included")
	cmd.Flags().IntVar(&n, "events", 5000, "how many recent events --all looks through")
	return cmd
}
