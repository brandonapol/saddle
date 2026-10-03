package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/brandonapol/saddle/internal/app"
)

// concurrencyCmd reads or overrides the worker cap at runtime (#176).
func concurrencyCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "concurrency [N|reset]",
		Short: "Show or change how many agents may run at once",
		Long: fmt.Sprintf(`With no argument, prints how many worker agents are running and the cap.
N (%d to %d) overrides the configured concurrency at runtime; the override
survives restarts until 'saddle concurrency reset' goes back to the config.
Spawn honors a change at once. Lowering the cap stops nothing that is
running: new spawns wait until fewer agents run than the cap.`, app.MinConcurrency, app.MaxConcurrency),
		Args: cobra.MaximumNArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			var c app.Concurrency
			var err error
			switch {
			case len(args) == 0:
				c, err = a.Concurrency()
			case args[0] == "reset":
				c, err = a.ResetConcurrency()
			default:
				n, perr := strconv.Atoi(args[0])
				if perr != nil {
					return fmt.Errorf("%q: want a number from %d to %d, or reset", args[0], app.MinConcurrency, app.MaxConcurrency)
				}
				c, err = a.SetConcurrency(n)
			}
			if err != nil {
				return err
			}
			return printConcurrency(cmd.OutOrStdout(), c, asJSON)
		}),
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func printConcurrency(out io.Writer, c app.Concurrency, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(c)
	}
	fmt.Fprintf(out, "bots: %d running, limit %d (%s)\n", c.Running, c.Limit, c.Source)
	if c.Running > c.Limit {
		fmt.Fprintf(out, "over the limit: nothing is stopped; new spawns wait until fewer than %d run\n", c.Limit)
	}
	return nil
}
