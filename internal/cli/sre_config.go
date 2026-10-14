package cli

import (
	"fmt"
	"os"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/sre"
	"github.com/spf13/cobra"
)

func sreCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sre",
		Short: "SRE mode: production checks from .saddle/sre.toml",
	}
	cmd.AddCommand(sreCheckConfigCmd())
	cmd.AddCommand(sreGuardCmds()...)
	return cmd
}

func sreCheckConfigCmd() *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:   "check-config",
		Short: "Validate .saddle/sre.toml",
		Long: `Parses and validates .saddle/sre.toml (docs/sre.md): named checks, endpoints
and credential references. Plaintext secrets are rejected; reference
credentials by env var name or ref instead. Exits non-zero on any problem.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if root == "" {
				wd, err := os.Getwd()
				if err != nil {
					return err
				}
				if root, err = gitx.Root(wd); err != nil {
					return err
				}
			}
			c, err := sre.Load(root)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s ok: %d check(s), %d endpoint(s), %d credential ref(s)\n",
				sre.File, len(c.Checks), len(c.Endpoints), len(c.Credentials))
			return nil
		},
	}
	cmd.Flags().StringVar(&root, "root", "", "repo root (default: the current repo)")
	return cmd
}
