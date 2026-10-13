package autopilot

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// The idle loop (#463). A stalled or empty pipeline nudges the orchestrator,
// and an orchestrator that only answers with status burns cycles all night
// while it acts on what it remembers. So the nudge carries saddle's own
// records (owner blockers with their age, tasks, PRs, the merge train),
// demands a lookahead step that produces non-blocked work, and the same
// idle state is nudged at most DefaultIdleRepeats times: after that saddle
// tells the owner once and stays quiet until something changes.

// DefaultIdleRepeats is how many nudges the same idle state gets before
// saddle stops nudging and escalates to the owner.
const DefaultIdleRepeats = 3

// snapshotCap is how many entries of each snapshot list a nudge shows.
const snapshotCap = 12

// Blocker is something only the owner can unblock.
type Blocker struct {
	What  string    // one line
	Since time.Time // when it started waiting; zero if unknown
}

// Snapshot is the pipeline as saddle's records have it now, one line per
// entry, so the orchestrator re-reads state instead of trusting memory.
type Snapshot struct {
	Blockers []Blocker
	Tasks    []string // live and queued workers with their status
	OpenPRs  []string // landed work whose PR is still open
	Merged   []string // recently merged work, newest first
	Queue    []string // the merge train: queued, held, failed
}

// idleKey fingerprints an idle state: the same key means nothing the
// orchestrator could act on has changed since the last nudge.
// The reason is left out: an escalation is itself an unread notice, and a
// "silent" nudge about it is the same idle state.
func idleKey(digest string, s Snapshot) string {
	var b strings.Builder
	b.WriteString(digest)
	bl := make([]string, len(s.Blockers))
	for i, x := range s.Blockers {
		bl[i] = x.What
	}
	slices.Sort(bl)
	for _, l := range [][]string{bl, s.Tasks, s.OpenPRs, s.Merged, s.Queue} {
		b.WriteString("|" + strings.Join(l, ";"))
	}
	return b.String()
}

// IdlePrompt is the continue nudge for an idle or stalled pipeline: why it
// is idle, the state digest, the snapshot, and the lookahead step the
// orchestrator must do before it replies. label is the ready label new
// tickets go under. It is one line, since it is typed into an input box.
func IdlePrompt(why, digest, label string, s Snapshot, now time.Time) string {
	var b strings.Builder
	b.WriteString("[saddle autopilot] continue: " + why + ". State: " + digest + ".")
	if len(s.Blockers) > 0 {
		var bs []string
		for _, x := range s.Blockers {
			bs = append(bs, x.What+" ("+age(x.Since, now)+")")
		}
		fmt.Fprintf(&b, " Owner-blocked (%d): %s.", len(bs), list(bs))
	}
	b.WriteString(" Saddle's records now, which beat what you remember (re-check before you tell the owner anything is held):")
	fmt.Fprintf(&b, " tasks: %s; open PRs: %s; merged: %s; train: %s.", list(s.Tasks), list(s.OpenPRs), list(s.Merged), list(s.Queue))
	b.WriteString(" Before you reply, look ahead: (1) enumerate the next tasks in the current and next epic;" +
		" (2) draft the ADRs, plans and tickets the owner will need to approve, marked Proposed only;" +
		" (3) pick the tickets that depend on no Proposed ADR and no pending owner answer, and spawn them or open them labelled " + label + "." +
		" Reply with the work you started, not a status: a status-only reply is a wasted cycle." +
		" If nothing qualifies, say once what you checked and what the owner must decide." +
		" Never ask the owner whether to continue.")
	return b.String()
}

// RunwayPrompt asks the orchestrator, once a run starts, for a short
// runway plan: what runs in the next hours, and the owner decisions it
// needs up front so the night isn't spent waiting on them.
func RunwayPrompt(st State, issues []Issue, s Snapshot, now time.Time) string {
	var ready []string
	for _, is := range issues {
		ready = append(ready, fmt.Sprintf("#%d %s", is.Number, is.Title))
	}
	var bs []string
	for _, x := range s.Blockers {
		bs = append(bs, x.What+" ("+age(x.Since, now)+")")
	}
	return "[saddle autopilot] autopilot is on (" + st.Goal() + ", ready label " + st.Label() + ")." +
		" Ready queue: " + list(ready) + ". Owner-blocked: " + list(bs) + "." +
		" Post the owner a short runway plan now: what will run in the next hours and in what order," +
		" which owner decisions (ADRs, dependencies, releases, permissions) you need up front, and what you will do meanwhile if they go unanswered." +
		" Then start the work; don't wait for a reply."
}

func list(l []string) string {
	if len(l) == 0 {
		return "none"
	}
	if len(l) > snapshotCap {
		return strings.Join(l[:snapshotCap], ", ") + fmt.Sprintf(" and %d more", len(l)-snapshotCap)
	}
	return strings.Join(l, ", ")
}

func age(since, now time.Time) string {
	if since.IsZero() {
		return "age unknown"
	}
	d := now.Sub(since).Round(time.Minute)
	if d < time.Minute {
		return "just now"
	}
	return "waiting " + strings.TrimSuffix(d.String(), "0s")
}
