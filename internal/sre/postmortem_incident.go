package sre

import (
	"fmt"
	"sort"
)

// PostmortemFromIncident builds the postmortem record for a closed incident.
// A root cause counts only if a different agent verified it and nobody refuted
// it; without one the record has no root cause and is written as unresolved.
func PostmortemFromIncident(inc *Incident) PostmortemRecord {
	rec := PostmortemRecord{Title: inc.Summary, Start: inc.Opened, End: inc.Closed}
	if rec.Title == "" {
		rec.Title = inc.ID
	}
	rec.Timeline = append(rec.Timeline, TimelineEntry{inc.Opened, fmt.Sprintf("Incident %s opened; investigators: %v", inc.ID, inc.Investigators)})
	var verified *Hypothesis
	for i := range inc.Hypotheses {
		h := &inc.Hypotheses[i]
		rec.Timeline = append(rec.Timeline, TimelineEntry{h.Stated, fmt.Sprintf("%s stated root cause %d: %s", h.By, h.ID, h.RootCause)})
		if h.Evidence != "" {
			rec.Findings = append(rec.Findings, LaneFinding{Lane: h.By, Summary: h.Evidence})
		}
		if h.Verified != nil {
			rec.Timeline = append(rec.Timeline, TimelineEntry{h.Verified.At, fmt.Sprintf("%s verified root cause %d", h.Verified.By, h.ID)})
			if verified == nil && h.Refuted == nil && h.Verified.By != h.By {
				verified = h
			}
		}
		if h.Refuted != nil {
			rec.Timeline = append(rec.Timeline, TimelineEntry{h.Refuted.At, fmt.Sprintf("%s refuted root cause %d: %s", h.Refuted.By, h.ID, h.Refuted.Evidence)})
		}
	}
	for _, p := range inc.Proposals {
		rec.FollowUps = append(rec.FollowUps, fmt.Sprintf("Proposed by %s against root cause %d: `%s`", p.By, p.Hypothesis, p.Command))
	}
	if !inc.Closed.IsZero() {
		rec.Timeline = append(rec.Timeline, TimelineEntry{inc.Closed, "Incident closed"})
	}
	sort.SliceStable(rec.Timeline, func(i, j int) bool { return rec.Timeline[i].At.Before(rec.Timeline[j].At) })
	if verified != nil {
		rec.RootCause = verified.RootCause
		rec.VerifiedBy = verified.Verified.By
		rec.Verified = verified.Verified.Evidence
	}
	return rec
}
