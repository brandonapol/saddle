package doctor

import (
	"context"
	"fmt"
	"strings"

	"github.com/brandonapol/saddle/internal/forge"
)

// CheckForgeFacts shows what the forge actually does for this repo (#822).
const CheckForgeFacts = "forge facts"

// forgeFacts reports the probed forge.Facts that merging and moving PRs act
// on. It only informs: a fact the probe can't read is named, and a forge
// that didn't answer is a warning to run doctor again.
func (r *run) forgeFacts() Result {
	if res, skip := r.skipped(CheckForgeFacts, nil, ""); skip {
		return res
	}
	base := r.base
	if base == "" {
		base = r.cfg.Base
	}
	f, err := forge.ReadFacts(context.Background(), r.forge(), base)
	if res, skip := r.skipped(CheckForgeFacts, err, ""); skip {
		return res
	}
	if err != nil {
		return warn(CheckForgeFacts, "could not probe: "+err.Error(), "run saddle doctor again once "+r.providerName()+" answers")
	}
	onOff := func(b bool) string {
		if b {
			return "on"
		}
		return "off"
	}
	parts := []string{
		"Stacked PRs " + onOff(f.StackedPRs),
		fmt.Sprintf("merge queue %s %s", onOff(f.MergeQueue), base),
		"auto-merge " + onOff(f.AutoMerge),
	}
	if len(f.RequiredChecks) > 0 {
		parts = append(parts, "required checks: "+strings.Join(f.RequiredChecks, ", "))
	} else {
		parts = append(parts, "no required checks")
	}
	detail := strings.Join(parts, "; ")
	if len(f.Unread) > 0 {
		detail += " (couldn't read " + strings.Join(f.Unread, "; ") + ")"
	}
	return ok(CheckForgeFacts, detail)
}
