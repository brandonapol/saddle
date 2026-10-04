package sentinel

import (
	"context"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/ciwatch"
)

// onRed gets the failing checks' log tails for a layer that newly went red
// and hands them to its repair; it returns what was done, for the notice.
func (c *CIRed) onRed(ctx context.Context, l app.CIRedLayer) string {
	var fails []app.CIRedFailure
	for _, f := range c.logs(ctx, l.PR) {
		fails = append(fails, app.CIRedFailure{Check: f.Check.Label(), RunURL: f.RunURL, Step: f.Step, LogTail: f.LogTail})
	}
	note, err := c.App.CIRedRepair(l.Task, fails)
	if err != nil {
		c.App.Store.Event(l.Task, EventCIRedError, "repair: "+err.Error())
	}
	return note
}

// logs fetches the failing checks of pr with their log tails: a fresh
// ciwatch watcher reports every check failing now.
func (c *CIRed) logs(ctx context.Context, pr string) []ciwatch.Failed {
	if c.Logs != nil {
		return c.Logs(ctx, pr)
	}
	w, err := ciwatch.New(ciwatch.Config{GH: c.GH, LogLines: 40,
		Targets: func(context.Context) ([]ciwatch.Target, error) { return []ciwatch.Target{{PR: pr}}, nil }})
	if err != nil {
		return nil
	}
	var out []ciwatch.Failed
	for _, e := range w.Poll(ctx) {
		if f, ok := e.(ciwatch.Failed); ok {
			out = append(out, f)
		}
	}
	return out
}
