package cli

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/spf13/cobra"

	"github.com/brandonapol/saddle/internal/app"
)

// event is one orchestrator notice as `saddle plugin events --json` prints
// it: kind "action" when the orchestrator must decide, "info" otherwise.
type event struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

type eventsOut struct {
	Events []event `json:"events"`
}

// pluginEventsCmd is the saddle mod's event feed (#170): it takes every
// pending orchestrator notice and prints it, so the mod decides which wake
// Claude and which only show in the UI. Taking them is what keeps the
// settings hook from delivering them a second time.
func pluginEventsCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:    "events",
		Short:  "Take the orchestrator's pending events, for the saddle mod",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !asJSON {
				return errors.New("saddle plugin events prints JSON only; pass --json")
			}
			a, why := openSession(wd())
			if a == nil {
				return errors.New(why)
			}
			defer a.Close()
			return writeEvents(cmd.OutOrStdout(), a)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// writeEvents takes and prints the orchestrator's notices, but only while
// the plugin engine or saddle up --rc runs: under plain saddle up they are
// the TUI orchestrator's.
func writeEvents(w io.Writer, a *app.App) error {
	out := eventsOut{Events: []event{}}
	if app.SessionWakes(a.LockOwner()) {
		ns, err := a.Store.TakeNotices(app.OrchestratorID, false)
		if err != nil {
			return err
		}
		for _, n := range ns {
			out.Events = append(out.Events, event{Kind: n.Kind, Text: n.Text})
		}
	}
	return json.NewEncoder(w).Encode(out)
}
