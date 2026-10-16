package app

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/forge"
	"github.com/brandonapol/saddle/internal/store"
)

// EventDigestSent records a digest line sent to the orchestrator; digest
// items logged after it go into the next one.
const EventDigestSent = "notice_digest_sent"

var (
	mergedPRRe   = regexp.MustCompile(`^Auto-merged (\S+) \(`)
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

// digestKinds are the counts a digest line can carry, in the order it shows
// them: the phrase for one and for many (%d is the count), and whether it
// lists ids.
var digestKinds = []struct {
	topic, one, many string
	ids              bool
}{
	{TopicMerged, "1 PR merged", "%d PRs merged", true},
	{TopicLanded, "1 task landed", "%d tasks landed", true},
	{TopicRestacked, "restacked once", "restacked %d times", false},
	{TopicLeft, "1 task left the stack", "%d tasks left the stack", false},
	{TopicCIGreen, "CI green on 1", "CI green on %d", false},
	{TopicCIFixing, "1 CI failure being fixed", "%d CI failures being fixed", false},
	{TopicStackClean, "stack checks clean", "stack checks clean (%dx)", false},
	{TopicSpawned, "1 sub-task spawned", "%d sub-tasks spawned", false},
	{TopicOther, "1 other update", "%d other updates", false},
}

// DigestHint says where the full list behind a digest line is. It isn't in
// the line itself: the TUI shows it once, folded (#402).
const DigestHint = "Full list: saddle notices --all"

// digest is the content of one digest line: counts and ids by topic since
// a clock time.
type digest struct {
	since string
	n     map[string]int
	ids   map[string][]string
}

func newDigest(since string) digest {
	return digest{since: since, n: map[string]int{}, ids: map[string][]string{}}
}

// String is the line, e.g. "Since 10:15: 3 PRs merged (#12 #13 #14), 2
// tasks landed (t1 t2), CI green on 1."
func (d digest) String() string {
	var parts []string
	for _, k := range digestKinds {
		n := d.n[k.topic]
		if n == 0 {
			continue
		}
		s := fmt.Sprintf(k.many, n)
		if n == 1 {
			s = k.one
		}
		if ids := d.ids[k.topic]; len(ids) > 0 {
			s += " (" + strings.Join(ids, " ") + ")"
		}
		parts = append(parts, s)
	}
	return fmt.Sprintf("Since %s: %s.", d.since, strings.Join(parts, ", "))
}

// summarizeDigest folds digest items into one line.
func summarizeDigest(items []store.Event) string {
	if len(items) == 0 {
		return ""
	}
	d := newDigest(items[0].TS.Format("15:04"))
	for _, e := range items {
		topic, text := splitDigestData(e.Data)
		d.n[topic]++
		switch topic {
		case TopicMerged:
			if m := mergedPRRe.FindStringSubmatch(text); m != nil {
				d.ids[topic] = append(d.ids[topic], forge.PRShort(m[1]))
			}
		case TopicLanded:
			if m := landedTaskRe.FindStringSubmatch(text); m != nil {
				d.ids[topic] = append(d.ids[topic], m[1])
			}
		}
	}
	return d.String()
}

var (
	digestLineRe  = regexp.MustCompile(`^Since (\d\d:\d\d): (.+)\.$`)
	digestPartRes = func() (rs [][2]*regexp.Regexp) {
		for _, k := range digestKinds {
			ids := ""
			if k.ids {
				ids = `(?: \(([^)]*)\))?`
			}
			many := strings.Replace(regexp.QuoteMeta(k.many), "%d", `(\d+)`, 1)
			rs = append(rs, [2]*regexp.Regexp{
				regexp.MustCompile(`^()` + regexp.QuoteMeta(k.one) + ids + `$`),
				regexp.MustCompile(`^` + many + ids + `$`),
			})
		}
		return rs
	}()
)

// parseDigest reads a line String wrote back. ok is false for anything else.
func parseDigest(line string) (d digest, ok bool) {
	m := digestLineRe.FindStringSubmatch(line)
	if m == nil {
		return d, false
	}
	d = newDigest(m[1])
	for _, part := range strings.Split(m[2], ", ") {
		matched := false
		for i, k := range digestKinds {
			for _, re := range digestPartRes[i] {
				pm := re.FindStringSubmatch(part)
				if pm == nil {
					continue
				}
				n := 1
				if pm[1] != "" {
					n, _ = strconv.Atoi(pm[1])
				}
				d.n[k.topic] += n
				if len(pm) > 2 && pm[2] != "" {
					d.ids[k.topic] = append(d.ids[k.topic], strings.Fields(pm[2])...)
				}
				matched = true
				break
			}
			if matched {
				break
			}
		}
		if !matched {
			return d, false
		}
	}
	return d, true
}

// IsDigest reports whether line is a digest line.
func IsDigest(line string) bool {
	_, ok := parseDigest(line)
	return ok
}

// MergeDigests folds two digest lines, the earlier first, into one rolling
// line that counts both since the earlier's time, e.g. "Since 09:23: 7 PRs
// merged, 1 task landed, restacked 7 times." ok is false unless both are
// digest lines.
func MergeDigests(earlier, later string) (string, bool) {
	a, ok := parseDigest(earlier)
	if !ok {
		return "", false
	}
	b, ok := parseDigest(later)
	if !ok {
		return "", false
	}
	for t, n := range b.n {
		a.n[t] += n
	}
	for t, ids := range b.ids {
		a.ids[t] = append(a.ids[t], ids...)
	}
	return a.String(), true
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

// NoticeLog returns the orchestrator notices the policy saw among the last
// n events, oldest first, with digests sent: interrupts, digest items,
// silenced ones and digest lines.
func (a *App) NoticeLog(n int) ([]store.Event, error) {
	es, err := a.Store.Events(n)
	if err != nil {
		return nil, err
	}
	var out []store.Event
	for _, e := range es {
		switch e.Kind {
		case EventNoticeInterrupt, EventNoticeDigest, EventNoticeSilent, EventDigestSent:
			out = append(out, e)
		}
	}
	return out, nil
}
