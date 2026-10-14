package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/sre"
	"github.com/spf13/cobra"
)

// sreGuardCmds are the read-only production guard's commands (#550):
// investigators state and verify root causes; the owner closes the incident.
func sreGuardCmds() []*cobra.Command {
	return []*cobra.Command{sreIncidentCmd(), sreRootCauseCmd(), sreVerifyCmd(false), sreVerifyCmd(true), sreCloseCmd()}
}

func sreIncidentCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "incident",
		Short: "Show the SRE incident: investigators, root causes and whether production is unlocked",
		Args:  cobra.NoArgs,
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			inc, err := sre.LoadIncident(a.Root)
			if err != nil {
				return err
			}
			printIncident(cmd.OutOrStdout(), inc)
			return nil
		}),
	}
}

func printIncident(w io.Writer, inc *sre.Incident) {
	if inc == nil {
		fmt.Fprintln(w, "No SRE incident.")
		return
	}
	state := "open"
	if !inc.Open() {
		state = "closed " + inc.Closed.Local().Format(time.DateTime)
	}
	fmt.Fprintf(w, "Incident %s (%s): %s\ninvestigators: %v\n", inc.ID, state, inc.Summary, inc.Investigators)
	if h, ok := inc.Unlocked(); ok {
		fmt.Fprintf(w, "production: actions may be proposed to the decision queue (root cause %d verified by %s)\n", h.ID, h.Verified.By)
	} else if inc.Open() {
		fmt.Fprintln(w, "production: read-only until a root cause is verified by a different agent")
	}
	for _, h := range inc.Hypotheses {
		fmt.Fprintf(w, "  %d. %s (stated by %s)\n", h.ID, h.RootCause, h.By)
		if h.Verified != nil {
			fmt.Fprintf(w, "     verified by %s: %s\n", h.Verified.By, h.Verified.Evidence)
		}
		if h.Refuted != nil {
			fmt.Fprintf(w, "     refuted by %s: %s\n", h.Refuted.By, h.Refuted.Evidence)
		}
	}
	for _, p := range inc.Proposals {
		fmt.Fprintf(w, "  proposed by %s: %s\n", p.By, p.Command)
	}
}

func sreRootCauseCmd() *cobra.Command {
	var task, evidence string
	cmd := &cobra.Command{
		Use:   "root-cause <cause>",
		Short: "State a root cause for the open incident; a different agent must verify it",
		Args:  cobra.ExactArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			by, err := resolveTask(a, task)
			if err != nil {
				return err
			}
			var id int
			if err := sre.UpdateIncident(a.Root, func(inc *sre.Incident) error {
				id, err = inc.State(by, args[0], evidence, time.Now())
				return err
			}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "root cause %d recorded. A different investigator must check it independently: saddle sre verify %d --evidence \"...\"\n", id, id)
			return nil
		}),
	}
	cmd.Flags().StringVar(&task, "task", "", "who states it (default: SADDLE_TASK or the current worktree)")
	cmd.Flags().StringVar(&evidence, "evidence", "", "what shows it")
	return cmd
}

func sreVerifyCmd(refute bool) *cobra.Command {
	var task, evidence string
	use, short := "verify", "Verify another agent's root cause independently; this unlocks proposing production actions"
	if refute {
		use, short = "refute", "Refute a root cause; production goes back to read-only"
	}
	cmd := &cobra.Command{
		Use:   use + " <id>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			id, err := strconv.Atoi(args[0])
			if err != nil {
				return fmt.Errorf("root cause id %q is not a number", args[0])
			}
			by, err := resolveTask(a, task)
			if err != nil {
				return err
			}
			var unlocked bool
			if err := sre.UpdateIncident(a.Root, func(inc *sre.Incident) error {
				if refute {
					err = inc.Refute(id, by, evidence, time.Now())
				} else {
					err = inc.Verify(id, by, evidence, time.Now())
				}
				_, unlocked = inc.Unlocked()
				return err
			}); err != nil {
				return err
			}
			if unlocked {
				fmt.Fprintf(cmd.OutOrStdout(), "root cause %d %sd. Production actions are unlocked: they go to the owner's decision queue, not run.\n", id, use)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "root cause %d %sd. Production stays read-only.\n", id, use)
			}
			return nil
		}),
	}
	cmd.Flags().StringVar(&task, "task", "", "who checks it (default: SADDLE_TASK or the current worktree)")
	cmd.Flags().StringVar(&evidence, "evidence", "", "what you checked, independently of the evidence given")
	return cmd
}

func sreCloseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "close",
		Short: "Close the SRE incident and lift the read-only guard (owner or orchestrator)",
		Args:  cobra.NoArgs,
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			by := os.Getenv("SADDLE_TASK")
			if err := sre.UpdateIncident(a.Root, func(inc *sre.Incident) error {
				if inc.Guarded(by) {
					return fmt.Errorf("%s investigates incident %s and can't close it; ask the orchestrator", by, inc.ID)
				}
				inc.Close(time.Now())
				return nil
			}); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "incident closed; the read-only guard is lifted")
			return nil
		}),
	}
}
