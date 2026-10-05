package app

import (
	"regexp"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// NoticeClass is how a notice for the orchestrator is delivered (#222).
type NoticeClass string

const (
	// NoticeInterrupt reaches the orchestrator at once: a question, a decision
	// only the owner can make, an escalation, a failure no automation handles.
	NoticeInterrupt NoticeClass = "interrupt"
	// NoticeDigest is routine progress, rolled into one digest line per
	// notices.digest_every.
	NoticeDigest NoticeClass = "digest"
	// NoticeSilent is logged only: a no-op, a repeat, or news status already shows.
	NoticeSilent NoticeClass = "silent"
)

// Events logging each orchestrator notice with its class; `saddle notices
// --all` reads them back.
const (
	EventNoticeInterrupt = "notice_interrupt"
	EventNoticeDigest    = "notice_digest"
	EventNoticeSilent    = "notice_silent"
)

// Digest topics: what a digested notice counts as in the summary line.
const (
	TopicMerged     = "merged"
	TopicLanded     = "landed"
	TopicRestacked  = "restacked"
	TopicLeft       = "left"
	TopicCIGreen    = "ci_green"
	TopicCIFixing   = "ci_fixing"
	TopicStackClean = "stack_clean"
	TopicSpawned    = "spawned"
	TopicOther      = "other"
)

type noticeRule struct {
	re    *regexp.Regexp
	class NoticeClass
	topic string
}

var (
	// asksOwner marks a notice that wants a decision, whatever its kind.
	asksOwner = regexp.MustCompile(`(?i)\bneeds you\b|\bdecide\b|\basks:|\?\s*$`)
	// noopRestack is a restack that moved nothing and dropped no one.
	noopRestack = regexp.MustCompile(`^Restacked \d+ landed tasks onto \S+ at \S+: 0 refs moved, \d+ commits already in base dropped\.( Merged, so skipped: [^.]*\.)?$`)
	leftMerged  = regexp.MustCompile(`^\S+ left the PR stack \(merged\)`)
	// digestRules map routine progress to a digest topic, first match wins.
	// Action notices only match the CI rules: a CI failure someone is
	// already fixing.
	digestRules = []noticeRule{
		{regexp.MustCompile(`^Auto-merged \S+ \(\S+\)`), NoticeDigest, TopicMerged},
		{regexp.MustCompile(`^Merged the stack `), NoticeDigest, TopicMerged},
		{regexp.MustCompile(`^Collapsed stack `), NoticeDigest, TopicMerged},
		{regexp.MustCompile(`^\S+ ".*" landed on \S+ at `), NoticeDigest, TopicLanded},
		{regexp.MustCompile(`^Restacked \d+ landed tasks`), NoticeDigest, TopicRestacked},
		{regexp.MustCompile(`^\S+ left the PR stack \(`), NoticeDigest, TopicLeft},
		{regexp.MustCompile(`^CI is passing again on `), NoticeDigest, TopicCIGreen},
		{regexp.MustCompile(`^CI on .* is green again`), NoticeDigest, TopicCIGreen},
		{regexp.MustCompile(`^The PR stack checks clean again`), NoticeDigest, TopicStackClean},
		{regexp.MustCompile(`^CI is red on .*No action needed`), NoticeDigest, TopicCIFixing},
		{regexp.MustCompile(`^Folded \S+ CI repair `), NoticeDigest, TopicCIFixing},
		{regexp.MustCompile(`^\S+ spawned sub-task `), NoticeDigest, TopicSpawned},
	}
	// ciHandled is a CI failure automation already handles: the owner was
	// told, or a fix task is on it.
	ciHandled = regexp.MustCompile(`(?s)^CI failed on .*\n(\S+ was told to fix it\.|\S+ is already fixing it\.|\S+ has landed, so I spawned \S+ to fix it\.)$`)
)

// ClassifyNotice decides how a notice for the orchestrator is delivered and,
// for a digest, what it counts as. autoMerge says whether auto-merge is on:
// then a PR leaving the stack because it merged is expected and silent.
// Action notices interrupt unless automation already handles them; info
// notices go to the digest unless they ask for a decision.
func ClassifyNotice(kind, text string, autoMerge bool) (NoticeClass, string) {
	text = strings.TrimSpace(text)
	if kind == store.NoticeAction {
		if ciHandled.MatchString(text) {
			return NoticeDigest, TopicCIFixing
		}
		return NoticeInterrupt, ""
	}
	if asksOwner.MatchString(text) {
		return NoticeInterrupt, ""
	}
	if noopRestack.MatchString(text) || (autoMerge && leftMerged.MatchString(text)) {
		return NoticeSilent, ""
	}
	for _, r := range digestRules {
		if r.re.MatchString(text) {
			return r.class, r.topic
		}
	}
	return NoticeDigest, TopicOther
}

// admitNotice is the notice policy's choke point, called first thing in
// Notify: it reports whether the notice should be queued for task now.
// Worker notices always are, unchanged. For the orchestrator it classifies
// the notice, logs it with its class, and admits only interrupts; digest
// items wait for FlushDigest, silent ones stay in the log. A repeat of a
// digest item within the digest window, or of an interrupt still waiting in
// the queue, is silent.
func (a *App) admitNotice(task, kind, text string) bool {
	if task != OrchestratorID {
		return true
	}
	class, topic := ClassifyNotice(kind, text, a.autoMergeOn())
	switch {
	case class == NoticeInterrupt && a.queuedAlready(text):
		class, topic = NoticeSilent, "repeat"
	case class == NoticeDigest && a.digestedRecently(text):
		class, topic = NoticeSilent, "repeat"
	}
	switch class {
	case NoticeInterrupt:
		a.Store.Event(OrchestratorID, EventNoticeInterrupt, text)
		return true
	case NoticeDigest:
		a.Store.Event(OrchestratorID, EventNoticeDigest, digestData(topic, text))
	default:
		a.Store.Event(OrchestratorID, EventNoticeSilent, digestData(topic, text))
	}
	return false
}

func (a *App) autoMergeOn() bool {
	st, err := a.AutomergeState()
	if err != nil {
		return a.Cfg.Train.AutoMerge
	}
	return st.Enabled
}

// queuedAlready reports whether text waits undelivered in the orchestrator's queue.
func (a *App) queuedAlready(text string) bool {
	ns, err := a.Store.PeekNotices(OrchestratorID, false)
	if err != nil {
		return false
	}
	for _, n := range ns {
		if n.Text == text {
			return true
		}
	}
	return false
}

// recentNoticeEvents bounds how far back the policy looks in the event log.
const recentNoticeEvents = 2000

// digestedRecently reports whether text went to the digest within the window.
func (a *App) digestedRecently(text string) bool {
	es, err := a.Store.Events(recentNoticeEvents)
	if err != nil {
		return false
	}
	cutoff := time.Now().Add(-a.digestEvery())
	for i := len(es) - 1; i >= 0 && !es[i].TS.Before(cutoff); i-- {
		if e := es[i]; e.Task == OrchestratorID && e.Kind == EventNoticeDigest {
			if _, t := splitDigestData(e.Data); t == text {
				return true
			}
		}
	}
	return false
}

func (a *App) digestEvery() time.Duration {
	if d := a.Cfg.Notices.DigestEvery; d > 0 {
		return d
	}
	return 15 * time.Minute
}

// digestData is a digest or silent event's data: "[topic] text".
func digestData(topic, text string) string {
	if topic == "" {
		return text
	}
	return "[" + topic + "] " + text
}

func splitDigestData(data string) (topic, text string) {
	if strings.HasPrefix(data, "[") {
		if i := strings.Index(data, "] "); i > 0 {
			return data[1:i], data[i+2:]
		}
	}
	return "", data
}
