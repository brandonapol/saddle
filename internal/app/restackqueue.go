package app

import (
	"fmt"
	"strings"

	"github.com/brandonapol/saddle/internal/store"
)

// EventRestackQueued is a layer restack left alone because GitHub refused
// the push to its PR head (GH006), as it does while the PR is in the merge
// queue (#431).
const EventRestackQueued = "restack_queued"

// queuedRefusal reports whether err is GitHub refusing a push with GH006,
// the protected-branch refusal a PR head in the merge queue gets.
func queuedRefusal(err error) bool {
	return err != nil && strings.Contains(err.Error(), "GH006")
}

// deferQueued records that restack left r and the layers above it unpushed
// because GitHub refused the push to r's branch. The local branches already
// sit on the new base, so the next restack pushes them once the queue lets
// go or the PR merges.
func (a *App) deferQueued(r restacked, err error) {
	a.Store.Event(r.ID, EventRestackQueued, err.Error())
	_ = a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf(
		"restack left %s's PR %s and the layers above it as they were: GitHub refused the push to %s (GH006), "+
			"as it does while a PR is in the merge queue. The next restack after it leaves the queue or merges publishes them.",
		r.ID, r.PR, r.Branch))
}
