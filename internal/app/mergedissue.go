package app

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/brandonapol/saddle/internal/store"
)

// EventIssueClosed is the events-table kind recording that saddle closed a
// merged task's issue itself (#572).
const EventIssueClosed = "issue_closed"

// closeMergedIssue is the backstop for #557 (#572). Stacked PR bodies say
// "Closes #N", but a PR body edited by hand, an issue named another way, or a
// template that stripped the line all leave the issue open after the merge.
// Once t's work is on base, closeMergedIssue closes the issue t was spawned
// with (issue=) if GitHub still has it open, with a comment saying how it
// merged: via, e.g. "its PR <url> was merged into main". It never touches any
// other issue. It returns a sentence for the orchestrator's notice, or ""
// when it closed nothing; GitHub errors only skip it.
func (a *App) closeMergedIssue(t store.Task, via string) string {
	if t.Issue <= 0 {
		return ""
	}
	n := strconv.Itoa(t.Issue)
	out, err := gh(a.Root, "issue", "view", n, "--json", "state")
	if err != nil {
		return ""
	}
	var is struct{ State string }
	if json.Unmarshal([]byte(out), &is) != nil || is.State != "OPEN" {
		return ""
	}
	comment := fmt.Sprintf("Saddle closed this: task %s worked on it and %s, but the merge left the issue open. "+
		"Reopen it if work remains.", t.ID, via)
	if _, err := gh(a.Root, "issue", "close", n, "--comment", comment); err != nil {
		a.Store.Event(t.ID, EventIssueClosed+"_failed", "#"+n+": "+err.Error())
		return ""
	}
	a.Store.Event(t.ID, EventIssueClosed, "#"+n+": "+via)
	return fmt.Sprintf(" Its issue #%d was still open, so saddle closed it.", t.Issue)
}
