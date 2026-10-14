package sre

import "time"

// PostmortemRecord is the record an SRE-mode incident ends with. It is the input to
// WritePostmortem and deliberately knows nothing about lanes or the guard.
type PostmortemRecord struct {
	Title string
	Start time.Time
	End   time.Time // zero while unresolved

	Timeline []TimelineEntry
	Findings []LaneFinding

	RootCause  string
	VerifiedBy string   // the agent that independently confirmed RootCause
	Verified   string   // how it was verified (evidence, commands run)
	FixPRs     []string // PR URLs or "#123" refs
	FollowUps  []string

	// FoundByChecks names the .saddle/sre.toml checks that located the cause;
	// they seed the proposed runbook additions.
	FoundByChecks []string
}

// TimelineEntry is one dated event in the incident.
type TimelineEntry struct {
	At   time.Time
	Text string
}

// LaneFinding is what one investigation lane reported.
type LaneFinding struct {
	Lane    string
	Summary string
}
