package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// glossaryHint is the last line of the root help.
const glossaryHint = "Saddle's vocabulary (restack, land, publish, stack, ...) is defined in docs/GLOSSARY.md."

// addGlossaryHelp makes the root help end with a pointer at the glossary.
// Subcommand help is untouched.
func addGlossaryHelp(root *cobra.Command) {
	def := root.HelpFunc()
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		def(cmd, args)
		if cmd == root {
			fmt.Fprintln(cmd.OutOrStdout(), "\n"+glossaryHint)
		}
	})
}
