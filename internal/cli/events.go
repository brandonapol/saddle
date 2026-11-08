package cli

import (
	"fmt"
	"strings"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/spf13/cobra"
)

func eventsCmd() *cobra.Command {
	var kinds []string
	var task string
	var n int
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Show saddle's event log, newest last, optionally filtered by kind or task",
		Long: `Every state change saddle makes is an event in .saddle/state.db: landed,
restack, gate runs, notices, watcher panics. This prints the last -n of them
(of that kind, with --kind), oldest first. --kind (repeatable) and --task narrow the list; a multi-line
event, such as a watcher_panic's stack, prints in full.

  saddle events --kind watcher_panic    why a watcher restarted
  saddle events --task t3 -n 50         what happened to t3`,
		Args: cobra.NoArgs,
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			out := cmd.OutOrStdout()
			es, err := a.Store.Events(n)
			if len(kinds) > 0 {
				es, err = a.Store.EventsOfKinds(kinds...)
			}
			if err != nil {
				return err
			}
			if len(kinds) > 0 && len(es) > n {
				es = es[len(es)-n:]
			}
			shown := 0
			for _, e := range es {
				if task != "" && e.Task != task {
					continue
				}
				shown++
				who := e.Task
				if who == "" {
					who = "-"
				}
				data := strings.ReplaceAll(strings.TrimRight(e.Data, "\n"), "\n", "\n    ")
				fmt.Fprintf(out, "%s %-5s %s %s\n", e.TS.Format("01-02 15:04:05"), who, e.Kind, data)
			}
			if shown == 0 {
				fmt.Fprintln(out, "No events match.")
			}
			return nil
		}),
	}
	cmd.Flags().StringSliceVar(&kinds, "kind", nil, "only events of this kind (repeatable), e.g. watcher_panic")
	cmd.Flags().StringVar(&task, "task", "", "only this task's events")
	cmd.Flags().IntVarP(&n, "n", "n", 200, "how many recent events to show at most")
	return cmd
}
