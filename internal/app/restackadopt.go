package app

import (
	"fmt"
	"strings"

	"github.com/brandonapol/saddle/internal/store"
)

// restackAdoptedEvent records commits on integration that restack matched to
// a landed task by patch-id though its train note didn't cover them (#392).
const restackAdoptedEvent = "restack_adopted"

// ownedPatchIDs maps the patch-id of each commit in a landed task's range to
// the task, in or out of the stack. A stacked task's commits are replayed with
// it; an unstacked one's are dropped as it left, so neither is unowned work.
func (a *App) ownedPatchIDs(all []landedTask) map[string]string {
	owned := map[string]string{}
	for _, l := range all {
		if l.From == "" || l.To == "" {
			continue
		}
		ids, _ := patchIDs(a.Root, l.From+".."+l.To)
		for _, id := range ids {
			if _, ok := owned[id]; !ok {
				owned[id] = l.ID
			}
		}
	}
	return owned
}

// adopt records, per task, the commits restack took as that task's although
// they sit after the last landed commit it could place on integration.
func (a *App) adopt(adopted map[string][]string) {
	for task, cs := range adopted {
		a.Store.Event(task, restackAdoptedEvent, strings.Join(cs, "\n"))
		_ = a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf(
			"restack adopted %s on %s for %s: their patch-ids match its landing, though its train note didn't cover them.",
			plural(len(cs), "commit"), a.Cfg.Integration, task))
	}
}
