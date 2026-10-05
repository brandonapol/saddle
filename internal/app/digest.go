package app

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// EventDigestSent records a digest line sent to the orchestrator; digest
// items logged after it go into the next one.
const EventDigestSent = "notice_digest_sent"

var (
	mergedPRRe   = regexp.MustCompile(`^Auto-merged (\S+) \(`)
	prNumberRe   = regexp.MustCompile(`/pull/(\d+)$`)
	landedTaskRe = regexp.MustCompile(`^(\S+) ".*" landed on `)
)

// pendingDigest returns the digest items logged since the last digest was
// sent, oldest first.
func (a *App) pendingDigest() ([]store.Event, error) {
	es, err := a.Store.Events(recentNoticeEvents)
	if err != nil {
		return nil, err
	}
	var out []store.Event
	for i := len(es) - 1; i >= 0; i-- {
		e := es[i]
		if e.Task != OrchestratorID {
			continue
		}
		if e.Kind == EventDigestSent {
			break
		}
		if e.Kind == EventNoticeDigest {
			out = append(out, e)
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// summarizeDigest folds digest items into one line, e.g. "since 10:15: 3 PRs
// merged (#12 #13 #14), 2 tasks landed (t1 t2), CI green on 1".
func summarizeDigest(items []store.Event) string {
	if len(items) == 0 {
		return ""
	}
	count := map[string]int{}
	var prs, landed []string
	for _, e := range items {
		topic, text := splitDigestData(e.Data)
		count[topic]++
		switch topic {
		case TopicMerged:
			if m := mergedPRRe.FindStringSubmatch(text); m != nil {
				if n := prNumberRe.FindStringSubmatch(m[1]); n != nil {
					prs = append(prs, "#"+n[1])
				} else {
					prs = append(prs, m[1])
				}
			}
		case TopicLanded:
			if m := landedTaskRe.FindStringSubmatch(text); m != nil {
				landed = append(landed, m[1])
			}
		}
	}
	var parts []string
	add := func(topic, one, many string, ids []string) {
		n := count[topic]
		if n == 0 {
			return
		}
		s := fmt.Sprintf(many, n)
		if n == 1 {
			s = one
		}
		if len(ids) > 0 {
			s += " (" + strings.Join(ids, " ") + ")"
		}
		parts = append(parts, s)
	}
	add(TopicMerged, "1 PR merged", "%d PRs merged", prs)
	add(TopicLanded, "1 task landed", "%d tasks landed", landed)
	add(TopicRestacked, "restacked once", "restacked %d times", nil)
	add(TopicLeft, "1 task left the stack", "%d tasks left the stack", nil)
	add(TopicCIGreen, "CI green on 1", "CI green on %d", nil)
	add(TopicCIFixing, "1 CI failure being fixed", "%d CI failures being fixed", nil)
	add(TopicStackClean, "stack checks clean", "stack checks clean (%dx)", nil)
	add(TopicSpawned, "1 sub-task spawned", "%d sub-tasks spawned", nil)
	add(TopicOther, "1 other update", "%d other updates", nil)
	return fmt.Sprintf("since %s: %s. Details: saddle notices --all", items[0].TS.Format("15:04"), strings.Join(parts, ", "))
}

// PendingDigest is the digest line that would be sent now, or "" when
// nothing happened since the last one.
func (a *App) PendingDigest() (string, error) {
	items, err := a.pendingDigest()
	if err != nil {
		return "", err
	}
	return summarizeDigest(items), nil
}

// FlushDigest sends the orchestrator one digest line of the routine notices
// held back since the last one, once the oldest has waited digest_every,
// the orchestrator is idle and the owner isn't typing. It goes in as an info
// notice, which never starts a turn by itself. It returns the line sent, or
// "" when it sent nothing.
func (a *App) FlushDigest(now time.Time) (string, error) {
	items, err := a.pendingDigest()
	if err != nil || len(items) == 0 {
		return "", err
	}
	if now.Sub(items[0].TS) < a.digestEvery() {
		return "", nil
	}
	if t := a.CompactTarget(); t != nil && (t.Busy() || t.Drafting()) {
		return "", nil
	}
	line := summarizeDigest(items)
	if err := a.Store.Notify(OrchestratorID, store.NoticeInfo, line); err != nil {
		return "", err
	}
	a.Store.Event(OrchestratorID, EventDigestSent, line)
	return line, nil
}

// RunDigest flushes the digest every 30 seconds until ctx ends.
func (a *App) RunDigest(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			if _, err := a.FlushDigest(now); err != nil {
				a.Store.Event(OrchestratorID, "error", "digest: "+err.Error())
			}
		}
	}
}
