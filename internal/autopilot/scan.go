package autopilot

import (
	"cmp"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"time"
)

// The ticket scan (#733). An orchestrator asked to "find work" answers with
// a status when every ticket it remembers looks blocked, and three such
// nudges used to send autopilot idle for the night. So autopilot reads the
// tracker itself: every open, unassigned ticket a maintainer opened, sorted
// by label (bugs first), and each nudge hands the orchestrator a concrete
// batch with suggested claims, its focus rotating through the categories.
// While the scan finds any eligible ticket the run neither stops nor goes
// idle.

// Scanner is an Env that lists the open issues nobody is assigned to that a
// maintainer opened, whatever their labels.
type Scanner interface {
	Unassigned() ([]Issue, error)
}

const (
	// DefaultScanEvery is how long a scan is reused before the next one.
	DefaultScanEvery = 5 * time.Minute
	// batchSize is how many tickets one nudge hands over.
	batchSize = 6
)

// category is a nudge focus and the label words that put a ticket in it.
type category struct {
	name  string
	words []string
}

// categories are the nudge focuses in rotation order; a ticket's rank is the
// first one its labels match, so bugs sort first.
var categories = []category{
	{"bugs", []string{"bug", "bugs", "regression", "crash", "defect"}},
	{"perf", []string{"perf", "performance", "speed", "slow", "latency"}},
	{"tests", []string{"test", "tests", "testing", "flaky", "coverage"}},
	{"docs", []string{"doc", "docs", "documentation", "readme"}},
	{"design drafts", []string{"design", "adr", "rfc", "proposal", "decision", "question"}},
}

// ownerChoice labels mark a ticket that waits only on the owner picking an option.
var ownerChoice = []string{"decision", "owner-decision", "needs-decision", "needs-design", "owner-choice", "question"}

func labelWords(l string) []string {
	return strings.FieldsFunc(strings.ToLower(l), func(r rune) bool {
		return r == '-' || r == ':' || r == '/' || r == ' ' || r == '_'
	})
}

// rank is the index of the first category is's labels match, or len(categories).
func rank(is Issue) int {
	for i, c := range categories {
		for _, l := range is.Labels {
			if slices.ContainsFunc(labelWords(l), func(w string) bool { return slices.Contains(c.words, w) }) {
				return i
			}
		}
	}
	return len(categories)
}

func waitsOnOwner(is Issue) bool {
	return slices.ContainsFunc(is.Labels, func(l string) bool { return slices.Contains(ownerChoice, strings.ToLower(l)) })
}

// eligibleTickets drops what autopilot or a task already handles: tickets
// with an open PR, a task on them, the needs-human label, or in skip.
func eligibleTickets(is []Issue, taken map[int]Task, skip map[int]bool) []Issue {
	var out []Issue
	for _, x := range is {
		if _, ok := taken[x.Number]; ok || x.HasPR || skip[x.Number] || x.hasLabel(NeedsHumanLabel) {
			continue
		}
		out = append(out, x)
	}
	return out
}

// rankTickets orders tickets for round: the round's focus first, then by
// rank (bugs first), then oldest first.
func rankTickets(is []Issue, round int) []Issue {
	focus := round % len(categories)
	out := slices.Clone(is)
	slices.SortStableFunc(out, func(a, b Issue) int {
		ra, rb := rank(a), rank(b)
		return cmp.Or(cmp.Compare(boolInt(ra != focus), boolInt(rb != focus)), cmp.Compare(ra, rb), cmp.Compare(a.Number, b.Number))
	})
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// BatchPrompt is the part of a nudge that hands over a ticket batch: the
// round's focus, up to batchSize tickets with their suggested claims, and
// for a ticket that only waits on an owner choice, the reversible way
// forward. It is one line.
func BatchPrompt(eligible []Issue, round int, repo fs.FS) string {
	if len(eligible) == 0 {
		return ""
	}
	focus := categories[round%len(categories)].name
	ranked := rankTickets(eligible, round)
	var lines []string
	for _, is := range ranked[:min(batchSize, len(ranked))] {
		l := fmt.Sprintf("#%d %s", is.Number, oneLine(is.Title))
		var notes []string
		if len(is.Labels) > 0 {
			notes = append(notes, strings.Join(is.Labels, ", "))
		}
		if c, inferred := TicketClaims(is.Body, repo); c != nil {
			how := "claims "
			if inferred {
				how = "inferred claims "
			}
			notes = append(notes, how+strings.Join(c, " "))
		} else {
			notes = append(notes, "no claims found: name them before you spawn")
		}
		if waitsOnOwner(is) {
			notes = append(notes, "waits on an owner choice: spawn its recommended option as a PR the owner can reject by closing it, and file the decision card in parallel")
		}
		lines = append(lines, l+" ("+strings.Join(notes, "; ")+")")
	}
	more := ""
	if n := len(ranked) - batchSize; n > 0 {
		more = fmt.Sprintf(" (%d more after these)", n)
	}
	return fmt.Sprintf(" Autopilot scanned the tracker: %d open unassigned maintainer %s%s. This round's focus: %s. Spawn this batch now, one task each with the suggested claims: %s.",
		len(eligible), plural(len(eligible), "ticket"), more, focus, strings.Join(lines, "; "))
}

// scan is the eligible unassigned tickets, reused for ScanEvery; none when
// the Env can't scan. A failed scan is logged and returned.
func (d *Driver) scan(now time.Time, l ledger, inQueue map[int]bool, st *State) ([]Issue, error) {
	sc, can := d.Env.(Scanner)
	if !can {
		return nil, nil
	}
	if d.scanAt.IsZero() || now.Sub(d.scanAt) >= orDefault(d.ScanEvery, DefaultScanEvery) || now.Before(d.scanAt) {
		is, err := sc.Unassigned()
		if err != nil {
			logErr(d.Env, "scan", err)
			return nil, err
		}
		d.scanned, d.scanAt = is, now
	}
	skip := map[int]bool{}
	for n := range inQueue {
		skip[n] = true // autopilot starts these itself
	}
	for _, n := range append(slices.Clone(st.GaveUp), st.Closed...) {
		skip[n] = true
	}
	return eligibleTickets(d.scanned, l.taken, skip), nil
}
