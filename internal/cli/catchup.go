package cli

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/mcpserver"
)

func catchupCmd() *cobra.Command {
	var (
		since  string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "catchup",
		Short: "Catch me up: what happened since I last looked, in at most 5 bullets",
		Long: `Say what happened since you last looked, in at most five plain bullets: what
waits on you first, then what is stuck, what got done, what started, and the
state now. "Last looked" is your last keystroke in the TUI; --since overrides
it with a duration back from now (2h) or an RFC 3339 time. Saddle builds it
from the event log; no model is asked.`,
		Args: cobra.NoArgs,
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			now := time.Now()
			from, err := mcpserver.CatchUpSince(a, since, now)
			if err != nil {
				return err
			}
			bs, err := a.CatchUp(from, now)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(mcpserver.CatchUpOut{Since: from, Bullets: bs})
			}
			fmt.Fprintf(out, "Since %s:\n", from.Local().Format("Mon 15:04"))
			for _, b := range bs {
				fmt.Fprintln(out, "- "+b)
			}
			return nil
		}),
	}
	cmd.Flags().StringVar(&since, "since", "", "start here instead of your last look: a duration back (2h) or an RFC 3339 time")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}
