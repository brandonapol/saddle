package cli

import (
	"fmt"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/spf13/cobra"
)

func configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Read or change one setting in .saddle/config.toml, keeping its comments",
		Long: `Reads or sets one dotted setting in this repo's .saddle/config.toml without
editing the file by hand (#908). A key saddle doesn't read, or a value it
rejects, leaves the file unchanged. A running saddle applies adapter and
train settings on its next spawn or land.

  saddle config set adapters.grok.disabled true
  saddle config set adapters.order '["claude", "codex"]'
  saddle config set train.lint.cmd make lint
  saddle config get adapters.policy`,
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "get <key>",
		Short: "Print a setting's value in .saddle/config.toml (empty when unset)",
		Args:  cobra.ExactArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			v, err := a.GetConfig(args[0])
			if err != nil {
				return err
			}
			if v == "" {
				fmt.Fprintf(cmd.OutOrStdout(), "%s is not set; saddle's default applies\n", args[0])
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), v)
			return nil
		}),
	}, &cobra.Command{
		Use:   "set <key> <value...>",
		Short: "Set a setting in .saddle/config.toml; the value is TOML, or a string",
		Args:  cobra.MinimumNArgs(2),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			value := args[1]
			for _, w := range args[2:] {
				value += " " + w
			}
			line, err := a.SetConfig(args[0], value)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "set %s in .saddle/config.toml\n", line)
			return nil
		}),
	})
	return cmd
}
