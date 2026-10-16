package app

import (
	"fmt"

	"github.com/brandonapol/saddle/internal/store"
)

// EventPRsQueued is a layer prs left unpublished because GitHub refused the
// push to its branch (GH006), as it does while its PR is in the merge queue
// (#433).
const EventPRsQueued = "prs_queued"

// deferQueuedPR records that prs left t and the layers stacked on it
// unpublished because GitHub refused the push to t's branch (see
// queuedRefusal). The next prs pushes them once the queue lets go or the PR
// merges.
func (a *App) deferQueuedPR(t store.Task, err error) {
	a.Store.Event(t.ID, EventPRsQueued, err.Error())
	what := t.ID + "'s branch"
	if t.PR != "" {
		what = t.ID + "'s PR " + t.PR
	}
	_ = a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf(
		"prs left %s and the layers stacked on it unpublished: %s refused the push to %s (GH006), "+
			"as it does while a PR is in the merge queue. The next prs after it leaves the queue or merges publishes them.",
		what, a.forgeName(), t.Branch))
}
