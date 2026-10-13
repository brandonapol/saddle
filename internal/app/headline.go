package app

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/automerge"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

// The state headline (#403) answers "is everything fine, or do I need to
// act?" in one sentence and a colour. Saddle computes it from its own
// state, so it costs no tokens and can't hallucinate. The TUI header, the
// plugin pane, saddle status and the remote snapshot all show the same one.

// Headline classes, highest priority first.
const (
	HeadlineNeedsYou = "needs_you"
	HeadlineBlocked  = "blocked"
	HeadlineDegraded = "degraded"
	HeadlineWorking  = "working"
	HeadlineIdle     = "idle"
)

// Headline colours.
const (
	ColorRed    = "red"
	ColorYellow = "yellow"
	ColorGreen  = "green"
	ColorGrey   = "grey"
)

// StateHeadline is the one-sentence state of the fleet.
type StateHeadline struct {
	Class string `json:"class" jsonschema:"needs_you, blocked, degraded, working or idle, highest priority first"`
	Color string `json:"color" jsonschema:"red, yellow, green or grey"`
	Text  string `json:"text"`
}

// HeadlineInput is the state a headline is computed from. It is plain data
// so the computation is a pure function.
type HeadlineInput struct {
	Now   time.Time
	Tasks []store.Task // every task; the orchestrator, landed and killed ones are skipped
	// Asks maps a needs-you task to its newest notification.
	Asks             map[string]store.Event
	Train            []store.TrainEntry
	CIRed            CIRedState
	Flag             *StackFlag // the stack sentinel's flag, nil when clear
	Exhausted        map[string]Exhaustion
	Usage            usage.State
	UsageNote        string // the worst window's name and percent
	AutomergeStopped string // why auto-merge stopped, "" when it runs or is off
	Merging          int    // PRs auto-merge can merge now
	Autopilot        bool
	// Orphans are the workers flagged done-orphaned (#467).
	Orphans []DoneOrphan
}

// maxAsk caps the quoted ask in a needs-you headline.
const maxAsk = 70

// Headline computes the headline: needs-you beats blocked beats degraded
// beats working beats idle.
func Headline(in HeadlineInput) StateHeadline {
	var live []store.Task
	for _, t := range in.Tasks {
		if t.ID != OrchestratorID && t.Active() {
			live = append(live, t)
		}
	}
	if h, ok := needsYouHeadline(in, live); ok {
		return h
	}
	if h, ok := blockedHeadline(in, live); ok {
		return h
	}
	if h, ok := degradedHeadline(in, live); ok {
		return h
	}
	return workingHeadline(in, live)
}

func needsYouHeadline(in HeadlineInput, live []store.Task) (StateHeadline, bool) {
	var waiting []store.Task
	for _, t := range live {
		if t.Status == store.NeedsYou {
			waiting = append(waiting, t)
		}
	}
	if len(waiting) == 0 {
		return StateHeadline{}, false
	}
	// Oldest ask first: it has waited longest.
	since := func(t store.Task) time.Time {
		if e, ok := in.Asks[t.ID]; ok {
			return e.TS
		}
		return in.Now
	}
	sort.SliceStable(waiting, func(i, j int) bool { return since(waiting[i]).Before(since(waiting[j])) })
	first := waiting[0]
	lead := "Needs you" + forDuration(in.Now.Sub(since(first)))
	h := StateHeadline{Class: HeadlineNeedsYou, Color: ColorRed}
	if len(waiting) == 1 {
		what := "is waiting at a prompt"
		if e, ok := in.Asks[first.ID]; ok && strings.TrimSpace(e.Data) != "" {
			what = "asks: " + clipText(e.Data, maxAsk)
		}
		h.Text = fmt.Sprintf("%s: %s %s.", lead, taskLabel(first), what)
	} else {
		ids := make([]string, len(waiting))
		for i, t := range waiting {
			ids[i] = taskLabel(t)
		}
		h.Text = fmt.Sprintf("%s: %d agents are waiting on you (%s).", lead, len(waiting), joinSome(ids, 3))
	}
	if held := heldBy(in, first.ID); held > 0 {
		h.Text += fmt.Sprintf(" %s above it %s waiting.", countOf(held, "layer"), isAre(held))
	}
	return h, true
}

func blockedHeadline(in HeadlineInput, live []store.Task) (StateHeadline, bool) {
	var reasons []string
	var oldest time.Time
	if f := in.Flag; f != nil && !f.Acked {
		reasons = append(reasons, fmt.Sprintf("the stack is at risk at %s (%s); run saddle restack", f.Task, clipText(f.Cause, 50)))
	}
	for _, l := range in.CIRed.Red {
		if l.Acked || (l.Repair != "" && !l.Escalated) {
			continue
		}
		why := "no fix is running"
		if l.Escalated {
			why = "the repairs ran out"
		}
		r := fmt.Sprintf("CI is red on %s and %s", layerLabel(l, live), why)
		if n := len(l.Held); n > 0 {
			r += fmt.Sprintf("; %s above it %s waiting", countOf(n, "layer"), isAre(n))
		}
		reasons = append(reasons, r)
		if oldest.IsZero() || (!l.Since.IsZero() && l.Since.Before(oldest)) {
			oldest = l.Since
		}
	}
	if in.AutomergeStopped != "" {
		reasons = append(reasons, "auto-merge stopped: "+clipText(in.AutomergeStopped, 60))
	}
	if len(reasons) == 0 {
		return StateHeadline{}, false
	}
	lead := "Blocked"
	if !oldest.IsZero() {
		lead += forDuration(in.Now.Sub(oldest))
	}
	return StateHeadline{Class: HeadlineBlocked, Color: ColorRed, Text: lead + ": " + joinReasons(reasons) + "."}, true
}

func degradedHeadline(in HeadlineInput, live []store.Task) (StateHeadline, bool) {
	var reasons []string
	for _, l := range in.CIRed.Red {
		if !l.Acked && l.Repair != "" && !l.Escalated {
			reasons = append(reasons, fmt.Sprintf("CI is red on %s and a fix is running", layerLabel(l, live)))
		}
	}
	if in.Usage == usage.Over {
		r := "the plan limit is used up"
		if in.UsageNote != "" {
			r += " (" + in.UsageNote + ")"
		}
		reasons = append(reasons, r)
	}
	names := make([]string, 0, len(in.Exhausted))
	for n := range in.Exhausted {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		reasons = append(reasons, fmt.Sprintf("%s is out of quota until %s", n, in.Exhausted[n].when()))
	}
	conflicts := 0
	for _, t := range live {
		if t.Status == store.Conflict {
			conflicts++
		}
	}
	if conflicts > 0 {
		reasons = append(reasons, fmt.Sprintf("%s %s resolving a conflict", countOf(conflicts, "agent"), isAre(conflicts)))
	}
	if n := len(in.Orphans); n > 0 {
		ids := make([]string, n)
		for i, o := range in.Orphans {
			ids[i] = o.Task
		}
		was, it := "was", "it"
		if n > 1 {
			was, it = "were", "them"
		}
		reasons = append(reasons, fmt.Sprintf("%s called done but %s never queued (%s); saddle is recovering %s",
			countOf(n, "agent"), was, joinSome(ids, 3), it))
	}
	if len(reasons) == 0 {
		return StateHeadline{}, false
	}
	return StateHeadline{Class: HeadlineDegraded, Color: ColorYellow, Text: "Slowed: " + joinReasons(reasons) + ". Nothing needs you yet."}, true
}

func workingHeadline(in HeadlineInput, live []store.Task) StateHeadline {
	running := 0
	for _, t := range live {
		if t.Status == store.Running || t.Status == store.Idle {
			running++
		}
	}
	queued := 0
	for _, e := range in.Train {
		if e.State == store.Queued {
			queued++
		}
	}
	var parts []string
	if running > 0 {
		parts = append(parts, countOf(running, "agent")+" running")
	}
	if queued > 0 {
		parts = append(parts, countOf(queued, "branch")+" queued to land")
	}
	if in.Merging > 0 {
		parts = append(parts, countOf(in.Merging, "PR")+" merging")
	}
	if len(parts) == 0 {
		if in.Autopilot {
			return StateHeadline{Class: HeadlineIdle, Color: ColorGrey, Text: "Idle: autopilot is on and waiting for ready work."}
		}
		return StateHeadline{Class: HeadlineIdle, Color: ColorGrey, Text: "Idle: no ready work. Add issues or run `saddle autopilot on`."}
	}
	return StateHeadline{Class: HeadlineWorking, Color: ColorGreen, Text: "Working: " + strings.Join(parts, ", ") + ". Nothing needs you."}
}

// heldBy counts the layers a red CI layer of task holds back.
func heldBy(in HeadlineInput, task string) int {
	if l, ok := in.CIRed.Layer(task); ok && !l.Acked {
		return len(l.Held)
	}
	return 0
}

// layerLabel names a red layer by its task and PR.
func layerLabel(l CIRedLayer, live []store.Task) string {
	if i := slices.IndexFunc(live, func(t store.Task) bool { return t.ID == l.Task }); i >= 0 {
		return taskLabel(live[i])
	}
	if n := prNumber(l.PR); n != "" {
		return l.Task + " (#" + n + ")"
	}
	return l.Task
}

// taskLabel names a task by its id and, when it has one, its PR or issue.
func taskLabel(t store.Task) string {
	switch {
	case prNumber(t.PR) != "":
		return t.ID + " (#" + prNumber(t.PR) + ")"
	case t.Issue > 0:
		return fmt.Sprintf("%s (#%d)", t.ID, t.Issue)
	}
	return t.ID
}

// prNumber is the number at the end of a PR URL or reference, or "".
func prNumber(pr string) string {
	pr = strings.TrimRight(strings.TrimSpace(pr), "/")
	if i := strings.LastIndexAny(pr, "/#"); i >= 0 {
		pr = pr[i+1:]
	}
	if pr == "" || strings.Trim(pr, "0123456789") != "" {
		return ""
	}
	return pr
}

// forDuration is " 2h" or " 15m" for how long something has waited, or ""
// under a minute.
func forDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return ""
	case d < time.Hour:
		return fmt.Sprintf(" %dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf(" %dh", int(d/time.Hour))
	}
	return fmt.Sprintf(" %dd", int(d/(24*time.Hour)))
}

// joinReasons keeps the first two reasons and counts the rest.
func joinReasons(rs []string) string {
	if len(rs) <= 2 {
		return strings.Join(rs, "; ")
	}
	return strings.Join(rs[:2], "; ") + fmt.Sprintf("; and %d more", len(rs)-2)
}

// joinSome lists up to n items and counts the rest.
func joinSome(xs []string, n int) string {
	if len(xs) <= n {
		return strings.Join(xs, ", ")
	}
	return strings.Join(xs[:n], ", ") + fmt.Sprintf(" +%d", len(xs)-n)
}

func countOf(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	if strings.HasSuffix(noun, "ch") {
		return fmt.Sprintf("%d %ses", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// clipText folds whitespace and clips s to n runes.
func clipText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return strings.TrimSpace(string(r[:n-1])) + "…"
	}
	return s
}

// headlineEvents is how far back the gatherer looks for needs-you asks.
const headlineEvents = 2000

// Headline reads saddle's local state and computes the headline. It runs
// no git and asks GitHub nothing, so the TUI can call it every refresh.
func (a *App) Headline(now time.Time) (StateHeadline, error) {
	in, err := a.headlineInput(now)
	if err != nil {
		return StateHeadline{}, err
	}
	return Headline(in), nil
}

func (a *App) headlineInput(now time.Time) (HeadlineInput, error) {
	in := HeadlineInput{Now: now, Asks: map[string]store.Event{}}
	var err error
	if in.Tasks, err = a.Store.Tasks(); err != nil {
		return in, err
	}
	evs, err := a.Store.Events(headlineEvents)
	if err != nil {
		return in, err
	}
	for _, e := range evs {
		if e.Kind == "notification" {
			in.Asks[e.Task] = e
		}
	}
	if in.Train, err = a.Store.Train(); err != nil {
		return in, err
	}
	if in.CIRed, err = a.CIRed(); err != nil {
		return in, err
	}
	f, flagged, err := a.Flag()
	if err != nil {
		return in, err
	}
	if flagged {
		in.Flag = &f
	}
	in.Exhausted = a.ExhaustedAdapters(now)
	e, err := a.Limits(now)
	if err != nil {
		return in, err
	}
	in.Usage = e.State
	for _, w := range []usage.WindowEstimate{e.FiveHour, e.Weekly} {
		if w.State == e.State && w.State != usage.OK {
			in.UsageNote = fmt.Sprintf("%s %.0f%%", w.Name, w.Percent*100)
			break
		}
	}
	am, err := a.AutomergeState()
	if err != nil {
		return in, err
	}
	in.AutomergeStopped, in.Merging = am.Stopped, merging(am)
	if in.Orphans, err = a.FlaggedOrphans(); err != nil {
		return in, err
	}
	if ap, err := a.AutopilotState(); err == nil {
		in.Autopilot = ap.On && !ap.Paused
	}
	return in, nil
}

// merging counts the stacks auto-merge would merge now.
func merging(am automerge.Status) int {
	if !am.Enabled || am.Stopped != "" {
		return 0
	}
	n := 0
	for _, s := range am.Stacks {
		if s.Ready && !s.Held {
			n++
		}
	}
	return n
}
