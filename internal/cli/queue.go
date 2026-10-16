package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/spf13/cobra"
)

// queueJSON is `saddle queue --json`: the waiting entries, next first, and
// who holds the engine lock ("engine", "up", or "" when nothing runs), so a
// reader like the Claude Code mod (#166) can tell whether saddle is live.
type queueJSON struct {
	Engine  string           `json:"engine"`
	Entries []queueEntryJSON `json:"entries"`
	// ForeignRuns are heavy-run leases no saddle agent took (a git hook or
	// another tool through saddle run), so a slow gate has a visible cause.
	ForeignRuns []foreignRunJSON `json:"foreign_runs"`
}

type foreignRunJSON struct {
	Class  string `json:"class"`
	State  string `json:"state"` // running or waiting
	Label  string `json:"label"`
	Repo   string `json:"repo,omitempty"`
	Cmd    string `json:"cmd"`
	PID    int    `json:"pid"`
	Weight int    `json:"weight"`
	AgeMS  int64  `json:"age_ms"`
}

// foreignRuns lists the machine queue's foreign leases, holders first. The
// heavy-run queue is advisory here: if it can't be read, there are none.
func foreignRuns(a *app.App) []foreignRunJSON {
	out := []foreignRunJSON{}
	q, _, err := a.Heavy().Open()
	if err != nil {
		return out
	}
	defer func() { _ = q.Close() }()
	st, err := q.Status()
	if err != nil {
		return out
	}
	for _, state := range []string{"running", "waiting"} {
		for _, cs := range st {
			es := cs.Holders
			if state == "waiting" {
				es = cs.Waiters
			}
			for _, e := range es {
				if e.Foreign {
					out = append(out, foreignRunJSON{Class: cs.Class, State: state, Label: e.Label, Repo: e.Repo, Cmd: e.Cmd,
						PID: e.PID, Weight: e.Weight, AgeMS: e.Age.Milliseconds()})
				}
			}
		}
	}
	return out
}

type queueEntryJSON struct {
	Position int    `json:"position"` // 1 lands next
	Task     string `json:"task"`
	State    string `json:"state"`
	Note     string `json:"note,omitempty"`
	Seq      int64  `json:"seq"`
	Attempts int    `json:"attempts,omitempty"`
}

// queueCmd shows and steers the merge train's waiting entries (#25).
func queueCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "queue",
		Short: "Show, reorder and hold the merge train's queue",
		Long: `Lists the branches waiting to land, next first. Subcommands move an entry,
hold it back without losing its place, and release it.

Below the train it lists foreign heavy runs: leases in the machine's
heavy-run queue that no saddle agent took (another repo's git hook or tool
through saddle run). They share the machine budget with the train's gates.`,
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			q, err := a.Queue()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			foreign := foreignRuns(a)
			if asJSON {
				js := queueJSON{Engine: a.LockOwner(), Entries: []queueEntryJSON{}, ForeignRuns: foreign}
				for i, e := range q {
					js.Entries = append(js.Entries, queueEntryJSON{Position: i + 1, Task: e.Task, State: e.State, Note: e.Note, Seq: e.Seq, Attempts: e.Attempts})
				}
				return writeJSON(out, js)
			}
			if len(q) == 0 {
				fmt.Fprintln(out, "the queue is empty")
			}
			for i, e := range q {
				line := fmt.Sprintf("%d. %s %s", i+1, e.Task, e.State)
				if e.Note != "" {
					line += ": " + e.Note
				}
				fmt.Fprintln(out, line)
			}
			if len(foreign) > 0 {
				fmt.Fprintln(out, "foreign heavy runs (not saddle agents; saddle runq status):")
			}
			for _, f := range foreign {
				who := f.Label
				if f.Repo != "" {
					who = f.Repo + "/" + who
				}
				fmt.Fprintf(out, "  %s %s  %s  pid %d  weight %d  %s (%s)\n", f.State, f.Class, who, f.PID, f.Weight, f.Cmd,
					(time.Duration(f.AgeMS) * time.Millisecond).Round(time.Second))
			}
			return nil
		}),
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.AddCommand(&cobra.Command{
		Use:   "move <task> <position>",
		Short: "Move a waiting entry to a position in the queue (1 lands next)",
		Args:  cobra.ExactArgs(2),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			pos, err := strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("position %q: want a number", args[1])
			}
			if err := a.MoveInQueue(args[0], pos); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "moved %s\n", args[0])
			return nil
		}),
	}, &cobra.Command{
		Use:   "hold <task> [reason...]",
		Short: "Keep a queued entry from landing until it is released",
		Args:  cobra.MinimumNArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			if err := a.Hold(args[0], strings.Join(args[1:], " ")); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is on hold; `saddle queue release %s` lets it land\n", args[0], args[0])
			return nil
		}),
	}, &cobra.Command{
		Use:   "release <task>",
		Short: "Let a held entry land again, from its place in the queue",
		Args:  cobra.ExactArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			if err := a.Unhold(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is back in line\n", args[0])
			return nil
		}),
	})
	return cmd
}
