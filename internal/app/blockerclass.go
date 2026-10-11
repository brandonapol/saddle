package app

// blockerKinds maps each escalation event kind to the blocker kind it files
// under (#407). It must agree with blockerFromEvent.
var blockerKinds = map[string]string{
	eventCIRedEscalatedKind: BlockerCIRed,
	eventHistoryCard:        BlockerHistoryCheck,
	eventRepairRefused:      BlockerRepairOfRepair,
	eventStackStuck:         BlockerStuckStack,
	eventTrainEscalated:     BlockerTrainEscalated,
}

// BlockerKindOf is the blocker kind an escalation event of kind files
// under, the class saddle doctor --predict (#408) weights its risks by.
// ok is false for an event that isn't an escalation.
func BlockerKindOf(eventKind string) (kind string, ok bool) {
	kind, ok = blockerKinds[eventKind]
	return kind, ok
}

// IsHistoryCheck reports whether check, a "workflow / job" label, reads
// every commit of a PR: a known history scanner or one of extra, the
// repo's [ci] history_checks (#405).
func IsHistoryCheck(check string, extra []string) bool { return isHistoryCheck(check, extra) }
