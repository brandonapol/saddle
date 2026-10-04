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
	link := &cobra.Command{
		Use:   "link",
		Short: "Publish the stacks and link each on GitHub as stacked PRs (gh stack link)",
		Long: `Runs prs, then links every stack of two or more PRs on GitHub with gh stack
link, by PR URL, bottom to top. prs and restack already do this with
[train] stack_backend = "gh-stack"; this does it now and says how each went.`,
		Args: cobra.NoArgs,
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			links, err := a.LinkStacks()
			if asJSON && err == nil {
				return writeJSON(cmd.OutOrStdout(), links)
			}
			out := cmd.OutOrStdout()
			if len(links) == 0 && err == nil {
				fmt.Fprintln(out, "no stack of two or more PRs to link")
			}
			for _, l := range links {
				if l.Error != "" {
					fmt.Fprintf(out, "%s: not linked: %s\n", strings.Join(l.Tasks, " → "), l.Error)
				} else {
					fmt.Fprintf(out, "%s: linked on GitHub\n", strings.Join(l.Tasks, " → "))
				}
			}
			return err
		}),
	}
	var method string
	merge := &cobra.Command{
		Use:   "merge <name|task|PR#>",
		Short: "Merge a whole stack atomically with gh stack merge, then restack",
		Long: `Links the stack holding the given custom stack, task or PR on GitHub and
merges every PR in it into base in one all-or-nothing gh stack merge, then
restacks so the merged tasks leave the PR stack. Needs [train] stack_backend
= "gh-stack", the gh-stack extension and a repo with Stacked PRs; otherwise
'saddle automerge on' merges stacks bottom-up.`,
		Args: cobra.ExactArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			res, err := a.MergeStack(args[0], method)
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), res)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "merged %s up to %s atomically into %s\n", strings.Join(res.Tasks, " → "), res.PR, a.Cfg.Base)
			if res.RestackErr != "" {
				fmt.Fprintf(out, "restack after the merge failed: %s\n", res.RestackErr)
			}
			return nil
		}),
	}
	merge.Flags().StringVar(&method, "method", "", "merge method: squash, rebase or merge (default: the repo's)")
	cmds := []*cobra.Command{create, add, remove, del, list, show, link, merge}
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
