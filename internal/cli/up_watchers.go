package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/app"
)

// writeWatchers prints one line naming when each background watcher last
// ticked (#789), when an engine has ever recorded any.
func writeWatchers(w io.Writer, a *app.App) {
	ws, err := a.Watchers(time.Now())
	if err != nil || len(ws) == 0 {
		return
	}
	parts := make([]string, len(ws))
	for i, h := range ws {
		parts[i] = h.Name + " " + h.Ago
		if h.Restarts > 0 {
			parts[i] += fmt.Sprintf(" (%d restarts)", h.Restarts)
		}
		if h.Stale {
			parts[i] += " (stale)"
		}
	}
	fmt.Fprintln(w, "watchers last ticked: "+strings.Join(parts, ", "))
}
