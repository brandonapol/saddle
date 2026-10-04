package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/spf13/cobra"
)

// customStackCmds are the `saddle stack` subcommands for owner-defined
// stacks (#211).
func customStackCmds() []*cobra.Command {
	var asJSON bool
	report := func(cmd *cobra.Command, rep app.StackReport, err error) error {
		if err != nil {
			return err
		}
		if asJSON {
			return writeJSON(cmd.OutOrStdout(), rep)
		}
		writeStackReport(cmd.OutOrStdout(), rep)
		return nil
	}
	create := &cobra.Command{
		Use:   "create [name] <task|PR#|branch>...",
		Short: "Make landed tasks one PR stack, bottom to top as given; link it on GitHub with gh-stack",
		Long: `Records an owner-defined stack in .saddle/stacks.json and publishes it: prs
and restack put each PR on the one before it, in the order given, even when
the tasks share no files, and the tasks leave automatic clustering. Every task
must have landed and still be in the PR stack, and be in no other custom
stack. The name is optional: the first argument is the name unless it names a
task, PR or branch; without one the stack is named after its bottom task.

With [train] stack_backend = "gh-stack" the stack is also linked on GitHub as
native stacked PRs (gh stack link). Without the gh-stack extension, or when
the repo doesn't have Stacked PRs, the stack is still recorded and published
the saddle way, by chaining PR bases.`,
		Example: `  saddle stack create ui t3 t5 t4
  saddle stack create t3 t5 '#212' saddle/t4-polish`,
		Args: cobra.MinimumNArgs(2),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			name, refs := a.StackArgs(args)
			rep, err := a.CreateStack(name, refs)
			return report(cmd, rep, err)
		}),
	}
	add := &cobra.Command{
		Use:   "add <name> <task|PR#|branch>...",
		Short: "Put landed tasks on top of a custom stack, in order",
		Args:  cobra.MinimumNArgs(2),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			rep, err := a.AddToStack(args[0], args[1:])
			return report(cmd, rep, err)
		}),
	}
	remove := &cobra.Command{
		Use:   "remove <name> <task|PR#|branch>...",
		Short: "Take tasks out of a custom stack; they go back to the automatic layout",
		Args:  cobra.MinimumNArgs(2),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			rep, err := a.RemoveFromStack(args[0], args[1:])
			return report(cmd, rep, err)
		}),
	}
	del := &cobra.Command{
		Use:   "delete <name>",
		Short: "Forget a custom stack; its tasks go back to the automatic layout",
		Long: `Forgets the stack and republishes, so its PRs are laid out automatically again.
A stack gh-stack linked on GitHub stays linked there: saddle only links and
never unstacks. Remove it in GitHub's stack UI if you want it gone.`,
		Args: cobra.ExactArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			rep, err := a.DeleteStack(args[0])
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), rep)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "deleted stack %s; its tasks are laid out automatically again\n", rep.Stack.Name)
			writePublish(out, rep)
			return nil
		}),
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List the custom stacks",
		Args:  cobra.NoArgs,
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			ss, err := a.CustomStacks()
			if err != nil {
				return err
			}
			if asJSON {
				if ss == nil {
					ss = []app.CustomStack{}
				}
				return writeJSON(cmd.OutOrStdout(), ss)
			}
			out := cmd.OutOrStdout()
			if len(ss) == 0 {
				fmt.Fprintln(out, "no custom stacks; `saddle stack create [name] <task>...` makes one")
				return nil
			}
			for _, s := range ss {
				fmt.Fprintf(out, "%s: %s\n", s.Name, strings.Join(s.Tasks, " → "))
			}
			return nil
		}),
	}
	show := &cobra.Command{
		Use:   "show <name|task|PR#>",
		Short: "Show a custom stack: its tasks bottom to top, their PRs and bases",
		Args:  cobra.ExactArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			v, err := a.ShowStack(args[0])
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), v)
			}
			writeStackView(cmd.OutOrStdout(), v)
			return nil
		}),
	}
	cmds := []*cobra.Command{create, add, remove, del, list, show}
	for _, c := range cmds {
		c.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	}
	return cmds
}

func writeJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func writeStackReport(out io.Writer, rep app.StackReport) {
	fmt.Fprintf(out, "stack %s: %s\n", rep.Stack.Name, strings.Join(rep.Stack.Tasks, " → "))
	writePublish(out, rep)
}

// writePublish says how the stack went out: PRs, link, fallback.
func writePublish(out io.Writer, rep app.StackReport) {
	if rep.PublishErr != "" {
		fmt.Fprintf(out, "recorded, but publishing stopped: %s\n", rep.PublishErr)
	} else if len(rep.PRs) > 0 {
		fmt.Fprintf(out, "published %d PRs\n", len(rep.PRs))
	}
	switch {
	case rep.Linked:
		fmt.Fprintln(out, "linked on GitHub as stacked PRs (gh-stack)")
	case rep.Note != "":
		fmt.Fprintln(out, rep.Note)
	case rep.Backend == "saddle":
		fmt.Fprintln(out, `chained by PR bases (stack_backend = "saddle"; set "gh-stack" under [train] to link it on GitHub too)`)
	}
}

func writeStackView(out io.Writer, v app.StackView) {
	fmt.Fprintf(out, "stack %s (bottom first)\n", v.Name)
	for i, m := range v.Members {
		fmt.Fprintf(out, "  %d. %s", i+1, m.Task)
		if m.Title != "" {
			fmt.Fprintf(out, " %q", m.Title)
		}
		if m.PR != "" {
			fmt.Fprintf(out, " %s", m.PR)
		}
		switch {
		case m.Base != "":
			fmt.Fprintf(out, " → %s", m.Base)
		case m.State != "":
			fmt.Fprintf(out, " (%s, out of the stack)", m.State)
		default:
			fmt.Fprint(out, " (not landed)")
		}
		fmt.Fprintln(out)
	}
	if v.GitHub != nil {
		fmt.Fprintf(out, "GitHub stack #%d (%d PRs)\n", v.GitHub.Number, len(v.GitHub.PRs))
	}
}
