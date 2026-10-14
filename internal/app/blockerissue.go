package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/brandonapol/saddle/internal/narrator"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/trust"
)

// Blocker issues (#407). Every blocker saddle escalates becomes a GitHub
// issue, or a comment on the one already open for it, so the same failure
// is recorded and counted instead of ending as a chat line. Issues are
// deduplicated by a signature, the blocker kind plus a normalized cause,
// kept in the issue body so it survives a state.db reset. The facts and
// the trigger come from saddle; the narrator (Haiku) only words the
// summary, and a template stands in when it can't.

// BlockerLabel marks every blocker issue; each also gets class:<kind>.
const BlockerLabel = "saddle-blocker"

// Blocker kinds.
const (
	BlockerCIRed          = "ci_red"           // CI still red after the repair attempts
	BlockerHistoryCheck   = "history_check"    // a history-scanning check saddle can't rewrite past
	BlockerRepairOfRepair = "repair_of_repair" // a CI repair went red itself
	BlockerStuckStack     = "stuck_stack"      // a red or conflicting layer nobody is fixing
	BlockerTrainEscalated = "train_escalated"  // a branch failed to land too often
)

// EventHandFix is the events-table kind for a hand fix recorded against a
// task (saddle blockers handfix). Its text goes into the task's blocker issue.
const EventHandFix = "hand_fix"

// eventStallRouted is written when the stall budget routes around a stuck
// layer. Its comment goes on the blocker issue already filed for the task,
// never a new one.
const eventStallRouted = "stall_routed"

// Events the blocker filer writes.
const (
	eventBlockerFiled = "blocker_filed" // data: signature #number
	eventBlockerSeen  = "blocker_seen"  // data: signature #number
	eventBlockerError = "blocker_error"
)

// Blocker is one escalated blockage.
type Blocker struct {
	Kind    string // one of the Blocker* kinds
	Cause   string // the raw cause; normalized into the signature
	Subject string // the title after the class, in plain words
	Task    string
	PR      string // the PR's number or URL, if any
	RunURL  string
	Detail  string // what saddle told the orchestrator
	// Timeline is what happened, oldest first: detected, repairs tried,
	// what the owner or orchestrator did.
	Timeline []BlockerStep
	HandFix  string // the exact hand fix, if one was recorded
	Area     string // the likely code area for the design section
	At       time.Time
}

// BlockerStep is one timeline entry.
type BlockerStep struct {
	At   time.Time
	What string
}

// Signature is b's dedup key.
func (b Blocker) Signature() string { return BlockerSignature(b.Kind, b.Cause) }

var (
	sigURL    = regexp.MustCompile(`https?://\S+`)
	sigSHA    = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
	sigRef    = regexp.MustCompile(`\bpr\s*#?\d+\b|#\d+\b|\bt\d+\b`)
	sigNum    = regexp.MustCompile(`\d+`)
	sigNonAln = regexp.MustCompile(`[^a-z]+`)
)

// sigWords caps how many words of one cause part reach the signature, so a
// long error message dedupes on its gist.
const sigWords = 6

// BlockerSignature is kind plus cause normalized: lowercased, with URLs,
// commit SHAs, PR and issue numbers, task IDs and other numbers dropped.
// Colons in the cause separate its parts, so "history_check: GitGuardian
// Security Checks" under ci_red gives ci_red:history_check:gitguardian_security_checks.
func BlockerSignature(kind, cause string) string {
	parts := []string{sigPart(kind)}
	for _, p := range strings.Split(sigURL.ReplaceAllString(cause, " "), ":") {
		p = strings.ToLower(p)
		p = sigSHA.ReplaceAllStringFunc(p, func(h string) string {
			if strings.ContainsAny(h, "0123456789") {
				return " "
			}
			return h // a word like "defaced", not a SHA
		})
		p = sigRef.ReplaceAllString(p, " ")
		p = sigNum.ReplaceAllString(p, " ")
		if s := sigPart(p); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ":")
}

func sigPart(s string) string {
	words := strings.Fields(sigNonAln.ReplaceAllString(strings.ToLower(s), " "))
	if len(words) > sigWords {
		words = words[:sigWords]
	}
	return strings.Join(words, "_")
}

// BlockerIssue is a blocker issue on GitHub.
type BlockerIssue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"` // OPEN or CLOSED
	URL    string `json:"url"`
}

// Signature is the signature stored in the issue's body, if any.
func (i BlockerIssue) Signature() string { return bodyMarker(i.Body, sigMarker) }

// Count is how many times the blocker was seen, from the issue's body.
func (i BlockerIssue) Count() int {
	n, err := strconv.Atoi(bodyMarker(i.Body, countMarker))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// BlockerGitHub is what the filer needs from GitHub. GHBlockers is the gh
// CLI; tests use a fake.
type BlockerGitHub interface {
	// Issues lists the blocker issues in state "open" or "all".
	Issues(state string) ([]BlockerIssue, error)
	Create(title, body string, labels []string) (BlockerIssue, error)
	Comment(number int, body string) error
	EditBody(number int, body string) error
	Reopen(number int) error
}

// GHBlockers is BlockerGitHub through the gh CLI in Dir.
type GHBlockers struct{ Dir string }

func (g GHBlockers) Issues(state string) ([]BlockerIssue, error) {
	out, err := gh(g.Dir, "issue", "list", "--label", BlockerLabel, "--state", state, "--limit", "500",
		"--json", "number,title,body,state,url")
	if err != nil {
		return nil, err
	}
	var is []BlockerIssue
	if err := json.Unmarshal([]byte(out), &is); err != nil {
		return nil, fmt.Errorf("parse gh issue list: %w", err)
	}
	return is, nil
}

func (g GHBlockers) Create(title, body string, labels []string) (BlockerIssue, error) {
	args := []string{"issue", "create", "--title", title, "--body", body}
	for _, l := range labels {
		// gh refuses an unknown label, so make sure each exists first.
		_, _ = gh(g.Dir, "label", "create", l, "--force", "--description", "Filed by saddle for a blocker (#407)")
		args = append(args, "--label", l)
	}
	out, err := gh(g.Dir, args...)
	if err != nil {
		return BlockerIssue{}, err
	}
	url := strings.TrimSpace(out[strings.LastIndex(out, "\n")+1:])
	n, _ := strconv.Atoi(url[strings.LastIndex(url, "/")+1:])
	return BlockerIssue{Number: n, Title: title, Body: body, State: "OPEN", URL: url}, nil
}

func (g GHBlockers) Comment(number int, body string) error {
	_, err := gh(g.Dir, "issue", "comment", strconv.Itoa(number), "--body", body)
	return err
}

func (g GHBlockers) EditBody(number int, body string) error {
	_, err := gh(g.Dir, "issue", "edit", strconv.Itoa(number), "--body", body)
	return err
}

func (g GHBlockers) Reopen(number int) error {
	_, err := gh(g.Dir, "issue", "reopen", strconv.Itoa(number))
	return err
}

// BlockerDrafter words a blocker's one-sentence summary. An error, or a nil
// drafter, means the template summary is used.
type BlockerDrafter func(ctx context.Context, b Blocker) (string, error)

// BlockerFiler files blocker issues.
type BlockerFiler struct {
	App   *App
	GH    BlockerGitHub
	Draft BlockerDrafter // nil: template only
	// Enabled reports whether filing is on: [issues] auto_file in a
	// trusted repo. Nil means always.
	Enabled func() bool
	Now     func() time.Time
}

// BlockerResult is what FileBlocker did.
type BlockerResult struct {
	Signature string
	Issue     BlockerIssue
	Created   bool // a new issue; false means it commented on one
	Skipped   bool // filing is off
}

// NewBlockerFiler is the filer over gh and the narrator, on when [issues]
// auto_file is set and the repo is trusted.
func (a *App) NewBlockerFiler() *BlockerFiler {
	var once sync.Once
	var n *narrator.Narrator
	draft := func(ctx context.Context, b Blocker) (string, error) {
		once.Do(func() { n = a.NarratorFromEnv(nil) })
		if n == nil {
			return "", errors.New("narrator is off")
		}
		return n.Draft(ctx, blockerDraftPrompt, blockerFacts(b), 200)
	}
	return &BlockerFiler{App: a, GH: GHBlockers{Dir: a.Root}, Draft: draft, Enabled: a.blockerFilingOn}
}

func (a *App) blockerFilingOn() bool {
	if !a.Cfg.Issues.AutoFile {
		return false
	}
	if trust.Inherited() {
		return true
	}
	r, err := trust.Identify(a.Root)
	if err != nil {
		return false
	}
	s, err := trust.Default()
	if err != nil {
		return false
	}
	ok, err := s.Trusted(r)
	return err == nil && ok
}

func (f *BlockerFiler) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// FileBlocker opens an issue for b, or comments on the one with its
// signature and bumps its count. A closed one is reopened rather than
// duplicated.
func (f *BlockerFiler) FileBlocker(ctx context.Context, b Blocker) (BlockerResult, error) {
	sig := b.Signature()
	res := BlockerResult{Signature: sig}
	if f.Enabled != nil && !f.Enabled() {
		res.Skipped = true
		return res, nil
	}
	if b.At.IsZero() {
		b.At = f.now()
	}
	all, err := f.GH.Issues("all")
	if err != nil {
		return res, err
	}
	i := slices.IndexFunc(all, func(is BlockerIssue) bool { return is.Signature() == sig })
	if i < 0 {
		summary := blockerTemplateSummary(b)
		if f.Draft != nil {
			if s, err := f.Draft(ctx, b); err == nil && strings.TrimSpace(s) != "" {
				summary = strings.TrimSpace(s)
			}
		}
		is, err := f.GH.Create(BlockerTitle(b), BlockerBody(b, summary, 1), []string{BlockerLabel, "class:" + b.Kind})
		if err != nil {
			return res, err
		}
		res.Issue, res.Created = is, true
		f.App.Store.Event(b.Task, eventBlockerFiled, fmt.Sprintf("%s #%d", sig, is.Number))
		return res, nil
	}
	is := all[i]
	n := is.Count() + 1
	if err := f.GH.Comment(is.Number, blockerSeenComment(b, n)); err != nil {
		return res, err
	}
	is.Body = setBodyMarker(is.Body, countMarker, strconv.Itoa(n))
	if err := f.GH.EditBody(is.Number, is.Body); err != nil {
		return res, err
	}
	if !strings.EqualFold(is.State, "open") {
		if err := f.GH.Reopen(is.Number); err != nil {
			return res, err
		}
		is.State = "OPEN"
	}
	res.Issue = is
	f.App.Store.Event(b.Task, eventBlockerSeen, fmt.Sprintf("%s #%d", sig, is.Number))
	return res, nil
}

// HandFix adds a hand fix to the issue filed for sig.
func (f *BlockerFiler) HandFix(sig, task, fix string) error {
	all, err := f.GH.Issues("all")
	if err != nil {
		return err
	}
	i := slices.IndexFunc(all, func(is BlockerIssue) bool { return is.Signature() == sig })
	if i < 0 {
		return fmt.Errorf("no blocker issue with signature %s", sig)
	}
	who := ""
	if task != "" {
		who = " on " + task
	}
	return f.GH.Comment(all[i].Number, fmt.Sprintf("Hand fix applied%s, %s:\n\n```\n%s\n```\n\nDesign a fix so this never needs a human again.",
		who, f.now().Format("2006-01-02"), fix))
}

// StallRouted comments the route-around recorded in data ("<pr url>
// held=<tasks> after=<duration>") on the issue filed for sig.
func (f *BlockerFiler) StallRouted(sig, task, data string) error {
	all, err := f.GH.Issues("all")
	if err != nil {
		return err
	}
	i := slices.IndexFunc(all, func(is BlockerIssue) bool { return is.Signature() == sig })
	if i < 0 {
		return fmt.Errorf("no blocker issue with signature %s", sig)
	}
	url, rest, _ := strings.Cut(data, " ")
	msg := fmt.Sprintf("Routed around %s", task)
	for _, kv := range strings.Fields(rest) {
		if v, ok := strings.CutPrefix(kv, "after="); ok && v != "" {
			msg += " after " + v
		}
	}
	msg += "; now a standalone PR " + url
	for _, kv := range strings.Fields(rest) {
		if v, ok := strings.CutPrefix(kv, "held="); ok && v != "" {
			msg += ". Held until it merges: " + v
		}
	}
	return f.GH.Comment(all[i].Number, msg+".")
}

// Body markers hold the signature and count as HTML comments, invisible on
// GitHub but parseable from the body alone.
const (
	sigMarker   = "saddle-blocker-signature"
	countMarker = "saddle-blocker-count"
)

func bodyMarker(body, name string) string {
	_, rest, ok := strings.Cut(body, "<!-- "+name+": ")
	if !ok {
		return ""
	}
	v, _, _ := strings.Cut(rest, " -->")
	return strings.TrimSpace(v)
}

func setBodyMarker(body, name, v string) string {
	line := "<!-- " + name + ": " + v + " -->"
	if old := bodyMarker(body, name); old != "" || strings.Contains(body, "<!-- "+name+": ") {
		return strings.Replace(body, "<!-- "+name+": "+old+" -->", line, 1)
	}
	return strings.TrimRight(body, "\n") + "\n" + line + "\n"
}

var blockerClass = map[string]string{
	BlockerCIRed:          "CI red",
	BlockerHistoryCheck:   "History check",
	BlockerRepairOfRepair: "Repair of a repair",
	BlockerStuckStack:     "Stuck stack",
	BlockerTrainEscalated: "Train escalation",
}

var blockerArea = map[string]string{
	BlockerCIRed:          "internal/app/cired.go, internal/app/cired_repair.go (CI repair loop)",
	BlockerHistoryCheck:   "internal/app/historyscan.go (history-scanning checks)",
	BlockerRepairOfRepair: "internal/app/historyscan.go, internal/app/cired_repair.go (repair lineage)",
	BlockerStuckStack:     "internal/app/stuckstack.go, internal/sentinel (stack health)",
	BlockerTrainEscalated: "internal/app/train.go (land retries and escalation)",
}

// BlockerTitle is the class plus the subject.
func BlockerTitle(b Blocker) string {
	class := blockerClass[b.Kind]
	if class == "" {
		class = strings.ReplaceAll(b.Kind, "_", " ")
	}
	subj := b.Subject
	if subj == "" {
		subj = b.Cause
	}
	return truncateRunes(class+": "+subj, 120)
}

// BlockerBody is a new issue's body.
func BlockerBody(b Blocker, summary string, count int) string {
	var s strings.Builder
	s.WriteString("**Difficulty: hard (design judgment).** Saddle filed this for a blocker it escalated; a lasting fix needs design judgment.\n\n")
	s.WriteString("## Summary\n" + summary + "\n\n")
	if b.Detail != "" {
		s.WriteString("What saddle saw:\n> " + strings.ReplaceAll(strings.TrimSpace(b.Detail), "\n", "\n> ") + "\n\n")
	}
	s.WriteString("## Timeline\n")
	steps := b.Timeline
	if len(steps) == 0 {
		steps = []BlockerStep{{At: b.At, What: "escalated"}}
	}
	for _, st := range steps {
		fmt.Fprintf(&s, "- %s %s\n", st.At.UTC().Format("2006-01-02 15:04Z"), st.What)
	}
	s.WriteString("\n## Hand fix\n")
	if b.HandFix != "" {
		s.WriteString("```\n" + strings.TrimSpace(b.HandFix) + "\n```\n\n")
	} else {
		s.WriteString("None recorded. Record one with `saddle blockers handfix <task> <what you did>`.\n\n")
	}
	if links := blockerLinks(b); links != "" {
		s.WriteString("## Links\n" + links + "\n")
	}
	s.WriteString("## Design a fix so this never needs a human\n")
	area := b.Area
	if area == "" {
		area = blockerArea[b.Kind]
	}
	if area != "" {
		s.WriteString("Likely code area: " + area + ".\n")
	}
	s.WriteString("Find why saddle could not route around this on its own, and make it do so.\n\n")
	fmt.Fprintf(&s, "<!-- %s: %s -->\n<!-- %s: %d -->\n", sigMarker, b.Signature(), countMarker, count)
	return s.String()
}

func blockerLinks(b Blocker) string {
	var s strings.Builder
	if b.Task != "" {
		s.WriteString("- Task: " + b.Task + "\n")
	}
	if b.PR != "" {
		s.WriteString("- PR: " + b.PR + "\n")
	}
	if b.RunURL != "" {
		s.WriteString("- Run: " + b.RunURL + "\n")
	}
	return s.String()
}

func blockerSeenComment(b Blocker, n int) string {
	parts := []string{b.At.Format("2006-01-02")}
	if b.Task != "" {
		parts = append(parts, b.Task)
	}
	if b.PR != "" {
		parts = append(parts, b.PR)
	}
	s := fmt.Sprintf("Seen again %s (%s so far).", strings.Join(parts, ", "), plural(n, "time"))
	if b.Detail != "" {
		s += "\n\n> " + strings.ReplaceAll(strings.TrimSpace(b.Detail), "\n", "\n> ")
	}
	if b.HandFix != "" {
		s += "\n\nHand fix:\n```\n" + strings.TrimSpace(b.HandFix) + "\n```"
	}
	return s
}

func blockerTemplateSummary(b Blocker) string {
	class := strings.ToLower(blockerClass[b.Kind])
	if class == "" {
		class = strings.ReplaceAll(b.Kind, "_", " ")
	}
	who := ""
	if b.Task != "" {
		who = " on " + b.Task
	}
	subj := b.Subject
	if subj == "" {
		subj = b.Cause
	}
	return fmt.Sprintf("Saddle escalated a %s blocker%s to the owner: %s.", class, who, strings.TrimSuffix(subj, "."))
}

const blockerDraftPrompt = `You write the summary line of a GitHub issue that records a blocker in saddle, a tool that runs parallel coding agents and lands their work. You get the facts saddle recorded. Write one plain sentence, no markdown, saying what got stuck and why, for a maintainer who will design a fix. Use only the facts given.`

func blockerFacts(b Blocker) string {
	var s strings.Builder
	fmt.Fprintf(&s, "Kind: %s\nSignature: %s\nSubject: %s\n", b.Kind, b.Signature(), b.Subject)
	if b.Task != "" {
		s.WriteString("Task: " + b.Task + "\n")
	}
	if b.Detail != "" {
		s.WriteString("What saddle told the orchestrator: " + b.Detail + "\n")
	}
	s.WriteString("Timeline:\n")
	for _, st := range b.Timeline {
		fmt.Fprintf(&s, "- %s %s\n", st.At.UTC().Format(time.RFC3339), st.What)
	}
	return s.String()
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// The event sweep. The escalation points already write an event when they
// escalate; FileBlockers turns each into a Blocker, so they need no edits
// and a blocker raised while gh is down is filed on a later sweep.

// blockerState is the sweep's cursor and the last signature filed per task,
// saved under .saddle.
type blockerState struct {
	After int64             `json:"after"`
	Last  map[string]string `json:"last,omitempty"` // task -> signature
}

func (a *App) blockerPath() string { return a.stateDir("blocker-issues.json") }

func (a *App) blockerState() (blockerState, bool) {
	var s blockerState
	b, err := os.ReadFile(a.blockerPath())
	ok := err == nil && json.Unmarshal(b, &s) == nil
	if s.Last == nil {
		s.Last = map[string]string{}
	}
	return s, ok
}

func (a *App) setBlockerState(s blockerState) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.blockerPath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.blockerPath())
}

// FileBlockers files an issue for each escalation event since the last
// sweep. The first sweep starts at the newest event, so history from before
// filing existed isn't filed. A gh failure stops the sweep at that event, to
// be retried next time.
func (f *BlockerFiler) FileBlockers(ctx context.Context) ([]BlockerResult, error) {
	a := f.App
	s, ok := a.blockerState()
	if !ok {
		id, err := a.Store.LastEventID()
		if err != nil {
			return nil, err
		}
		s.After = id
		return nil, a.setBlockerState(s)
	}
	if f.Enabled != nil && !f.Enabled() {
		// Off: move the cursor so turning it back on doesn't file a backlog.
		if id, err := a.Store.LastEventID(); err == nil && id > s.After {
			s.After = id
			return nil, a.setBlockerState(s)
		}
		return nil, nil
	}
	es, err := a.Store.EventsSince(s.After)
	if err != nil {
		return nil, err
	}
	var out []BlockerResult
	var ferr error
	for _, e := range es {
		if e.Kind == EventHandFix {
			// One recorded before the blocker is in its body already.
			if sig := s.Last[e.Task]; sig != "" {
				if err := f.HandFix(sig, e.Task, e.Data); err != nil {
					ferr = err
					break
				}
			}
			s.After = e.ID
			continue
		}
		if e.Kind == eventStallRouted {
			if sig := s.Last[e.Task]; sig != "" {
				if err := f.StallRouted(sig, e.Task, e.Data); err != nil {
					ferr = err
					break
				}
			}
			s.After = e.ID
			continue
		}
		b, ok := a.blockerFromEvent(e)
		if ok {
			res, err := f.FileBlocker(ctx, b)
			if err != nil {
				a.Store.Event(e.Task, eventBlockerError, err.Error())
				ferr = err
				break
			}
			if !res.Skipped {
				out = append(out, res)
				if e.Task != "" {
					s.Last[e.Task] = res.Signature
				}
			}
		}
		s.After = e.ID
	}
	if err := a.setBlockerState(s); err != nil {
		return out, err
	}
	return out, ferr
}

// blockerFromEvent turns an escalation event into a Blocker.
func (a *App) blockerFromEvent(e store.Event) (Blocker, bool) {
	b := Blocker{Task: e.Task, At: e.TS}
	red := a.ciRedLayer(e.Task)
	if red != nil {
		b.PR = red.PR
	}
	checks := ""
	if red != nil {
		checks = strings.Join(sortedCopy(red.Checks), ", ")
	}
	switch e.Kind {
	case eventCIRedEscalatedKind:
		b.Kind, b.Cause = BlockerCIRed, checks
		b.Subject = fmt.Sprintf("%s still red on %s's PR after %s", orUnknown(checks), e.Task, e.Data)
		b.Detail = fmt.Sprintf("CI on %s's PR %s stayed red after %s; saddle stopped spawning repairs.", e.Task, b.PR, e.Data)
	case eventHistoryCard:
		b.Kind, b.Cause = BlockerHistoryCheck, checks
		b.Subject = fmt.Sprintf("%s red on %s's unmerged layer", orUnknown(checks), e.Task)
		b.Detail = e.Data
	case eventRepairRefused:
		b.Kind, b.Cause = BlockerRepairOfRepair, "ci repair"
		b.Subject = fmt.Sprintf("CI repair %s for %s went red itself", e.Task, e.Data)
		b.Detail = fmt.Sprintf("%s is a CI repair for %s and its PR went red; saddle refused a repair of a repair.", e.Task, e.Data)
	case eventStackStuck:
		_, reason, _ := strings.Cut(e.Data, "|")
		b.Kind, b.Cause = BlockerStuckStack, reason
		if reason == StuckCIRed && checks != "" {
			b.Cause += ": " + checks
		}
		b.Subject = fmt.Sprintf("%s at layer %s with nobody fixing it", reason, e.Task)
		b.Detail = "Stack " + e.Data + " stayed stuck past [train] stuck_after."
	case eventTrainEscalated:
		why, _, _ := strings.Cut(e.Data, "; escalated after")
		b.Kind, b.Cause = BlockerTrainEscalated, why
		b.Subject = fmt.Sprintf("%s failed to land: %s", e.Task, truncateRunes(why, 70))
		b.Detail = e.Data
	default:
		return b, false
	}
	b.Timeline, b.HandFix = a.blockerTimeline(e)
	return b, true
}

// Event kinds the sweep turns into blockers, written by the escalation points.
const (
	eventCIRedEscalatedKind = eventCIRedEscalated
	eventStackStuck         = "stack_stuck"
	eventTrainEscalated     = "train_escalated"
)

func (a *App) ciRedLayer(task string) *CIRedLayer {
	if task == "" {
		return nil
	}
	s, err := a.CIRed()
	if err != nil {
		return nil
	}
	if i := slices.IndexFunc(s.Red, func(l CIRedLayer) bool { return l.Task == task }); i >= 0 {
		return &s.Red[i]
	}
	return nil
}

// blockerTimeline is the task's recent events up to and including e, and
// the last hand fix recorded for the task before it.
func (a *App) blockerTimeline(e store.Event) (out []BlockerStep, fix string) {
	if e.Task != "" {
		es, _ := a.Store.Events(400)
		var fixAt int64
		for _, x := range es {
			if x.Task != e.Task || x.ID >= e.ID || strings.HasPrefix(x.Kind, "blocker_") || x.Kind == EventNarratorError {
				continue
			}
			if x.Kind == EventHandFix && x.ID > fixAt {
				fix, fixAt = x.Data, x.ID
			}
			out = append(out, BlockerStep{At: x.TS, What: x.Kind + ": " + truncateRunes(x.Data, 160)})
		}
		slices.SortFunc(out, func(p, q BlockerStep) int { return p.At.Compare(q.At) })
		if len(out) > 12 {
			out = out[len(out)-12:]
		}
	}
	return append(out, BlockerStep{At: e.TS, What: e.Kind + ": " + truncateRunes(e.Data, 160) + " (escalated)"}), fix
}

func sortedCopy(s []string) []string {
	c := slices.Clone(s)
	slices.Sort(c)
	return c
}

func orUnknown(s string) string {
	if s == "" {
		return "CI"
	}
	return s
}

// OpenBlockers lists the open blocker issues, most seen first.
func OpenBlockers(g BlockerGitHub) ([]BlockerIssue, error) {
	is, err := g.Issues("open")
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(is, func(p, q BlockerIssue) int {
		if p.Count() != q.Count() {
			return q.Count() - p.Count()
		}
		return p.Number - q.Number
	})
	return is, nil
}
