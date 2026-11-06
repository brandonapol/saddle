package cli

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/spf13/cobra"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/store"
)

// event is one orchestrator notice as `saddle plugin events --json` prints
// it: kind "action" when the orchestrator must decide, "info" otherwise.
// ID is set under --peek, for the --ack that follows.
type event struct {
	ID   int64  `json:"id,omitempty"`
	Kind string `json:"kind"`
	Text string `json:"text"`
}

type eventsOut struct {
	Events []event `json:"events"`
	// Cards lists the open decision cards waiting on the owner, for the
	// wake prompt to carry (#793); empty when none is open.
	Cards string `json:"cards,omitempty"`
}

// pluginEventsCmd is the saddle mod's event feed (#170): it prints every
// pending orchestrator notice, so the mod decides which wake Claude and
// which only show in the UI. Plain --json takes them; --peek leaves them
// pending, with ids, and --ack marks those ids delivered once the mod's
// prompt went in (#793), so a submit that fails loses nothing. Taking them
// is what keeps the settings hook from delivering them a second time.
func pluginEventsCmd() *cobra.Command {
	var asJSON, peek bool
	var ack []int64
	cmd := &cobra.Command{
		Use:    "events",
		Short:  "Take the orchestrator's pending events, for the saddle mod",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !asJSON && len(ack) == 0 {
				return errors.New("saddle plugin events prints JSON only; pass --json")
			}
			a, why := openSession(wd())
			if a == nil {
				return errors.New(why)
			}
			defer a.Close()
			if len(ack) > 0 {
				return ackEvents(a, ack)
			}
			return writeEventsMode(cmd.OutOrStdout(), a, peek)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().BoolVar(&peek, "peek", false, "leave the events pending and print their ids, for a later --ack")
	cmd.Flags().Int64SliceVar(&ack, "ack", nil, "mark these event ids delivered")
	return cmd
}

// writeEvents takes and prints the orchestrator's notices, but only while
// the plugin engine or saddle up --rc runs: under plain saddle up they are
// the TUI orchestrator's.
func writeEvents(w io.Writer, a *app.App) error { return writeEventsMode(w, a, false) }

// writeEventsMode is writeEvents; peek leaves the notices pending.
func writeEventsMode(w io.Writer, a *app.App, peek bool) error {
	out := eventsOut{Events: []event{}}
	if app.SessionWakes(a.LockOwner()) {
		read := a.Store.TakeNotices
		if peek {
			read = a.Store.PeekNotices
		}
		ns, err := read(app.OrchestratorID, false)
		if err != nil {
			return err
		}
		for _, n := range ns {
			e := event{Kind: n.Kind, Text: n.Text}
			if peek {
				e.ID = n.ID
			}
			out.Events = append(out.Events, e)
		}
		out.Cards = a.OpenCardsReminder()
	}
	return json.NewEncoder(w).Encode(out)
}

// ackEvents marks the given notice ids delivered.
func ackEvents(a *app.App, ids []int64) error {
	ns := make([]store.Notice, len(ids))
	for i, id := range ids {
		ns[i] = store.Notice{ID: id}
	}
	return a.Store.MarkDelivered(ns)
}
