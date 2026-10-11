package remote

import (
	"cmp"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

// Source is the state the remote server reads. AppSource reads a repo;
// tests use a fake.
type Source interface {
	Snapshot() (Snapshot, error)
	NeedsYou() ([]NeedsYouItem, error)
	// Peek returns the last lines of a task's terminal, raw. The server
	// caps, scrubs and fences it (FencePeek).
	Peek(task string, lines int) (string, error)
}

// Snapshot is a compact view of a running saddle, small enough for a phone:
// no worktree paths, no window ids, live tasks only.
type Snapshot struct {
	Integration string                 `json:"integration"`
	Counts      Counts                 `json:"counts"`
	AutoMerge   string                 `json:"automerge,omitempty" jsonschema:"on, off or stopped"`
	StackAtRisk *mcpserver.StackRisk   `json:"stack_at_risk,omitempty"`
	CIRed       []app.CIRedHold        `json:"ci_red,omitempty"`
	Warnings    []string               `json:"warnings,omitempty"`
	Tasks       []TaskLine             `json:"tasks" jsonschema:"live tasks, needs-you first"`
	Context     *mcpserver.OrchContext `json:"orchestrator_context,omitempty"`
	Limits      *PlanLimits            `json:"limits,omitempty" jsonschema:"plan-limit usage in the 5h and weekly windows"`
	Stacks      []string               `json:"stacks,omitempty" jsonschema:"the PR stack bottom up from the base, the merge train queue in landing order, and named stacks, one line each"`
}

// PlanLimits is plan-limit usage, compact.
type PlanLimits struct {
	State          string     `json:"state" jsonschema:"ok, warn or over: the worst window"`
	LaunchesPaused bool       `json:"launches_paused,omitempty" jsonschema:"new spawns are held until a window resets"`
	FiveHour       PlanWindow `json:"five_hour"`
	Weekly         PlanWindow `json:"weekly"`
}

// PlanWindow is one plan-limit window.
type PlanWindow struct {
	Percent   int     `json:"percent" jsonschema:"of the cap; 0 when unlimited"`
	State     string  `json:"state"`
	Tokens    int64   `json:"tokens"`
	USD       float64 `json:"usd"`
	ResetIn   string  `json:"reset_in,omitempty"`
	Unlimited bool    `json:"unlimited,omitempty"`
}

// LimitsFrom compacts a plan-limit estimate.
func LimitsFrom(e usage.LimitEstimate) *PlanLimits {
	line := func(w usage.WindowEstimate) PlanWindow {
		l := PlanWindow{State: w.State.String(), Tokens: w.Tokens.Total(), USD: w.USD, Unlimited: w.Unlimited}
		if !w.Unlimited {
			l.Percent = int(w.Percent*100 + 0.5)
		}
		if w.ResetIn > 0 {
			l.ResetIn = strings.TrimSuffix(w.ResetIn.Round(time.Minute).String(), "0s")
		}
		return l
	}
	return &PlanLimits{State: e.State.String(), LaunchesPaused: e.ShouldPauseLaunches(), FiveHour: line(e.FiveHour), Weekly: line(e.Weekly)}
}

// StackGraph draws the stacks as short lines: the PR stack bottom up from
// base ("stack: main ← t1 #11 ← t3 #13"), the merge train queue in landing
// order, and each named stack. The layer at risk and CI-red PRs are marked.
func StackGraph(base string, train []store.TrainEntry, tasks []mcpserver.TaskView, custom []app.CustomStack, risk string, cired []app.CIRedHold) []string {
	byID := map[string]mcpserver.TaskView{}
	for _, t := range tasks {
		byID[t.ID] = t
	}
	red := map[string]bool{}
	for _, h := range cired {
		red[h.Task] = true
	}
	layer := func(id string) string {
		s := id
		t := byID[id]
		if t.PR != "" {
			s += " #" + path.Base(t.PR)
		}
		if red[id] {
			s += " (ci red)"
		}
		if id == risk {
			s += " (at risk)"
		}
		return s
	}
	var out, stack, queue []string
	for _, e := range train {
		switch e.State {
		case store.TrainOK:
			// The train's own rule (landedTask.stacked): a killed task
			// with a PR has left the stack.
			if t := byID[e.Task]; t.Status == store.Killed && t.PR != "" {
				continue
			}
			stack = append(stack, layer(e.Task))
		case store.Queued:
			queue = append(queue, e.Task)
		case store.OnHold:
			queue = append(queue, e.Task+" (held)")
		}
	}
	if len(stack) > 0 {
		out = append(out, "stack: "+strings.Join(append([]string{base}, stack...), " ← "))
	}
	if len(queue) > 0 {
		out = append(out, "queue: "+strings.Join(queue, ", "))
	}
	for _, c := range custom {
		ls := make([]string, len(c.Tasks))
		for i, id := range c.Tasks {
			ls[i] = layer(id)
		}
		out = append(out, fmt.Sprintf("stack %s: %s", c.Name, strings.Join(ls, " ← ")))
	}
	return out
}

// Counts are the headline numbers.
type Counts struct {
	Running  int `json:"running"`
	NeedsYou int `json:"needs_you"`
	Queued   int `json:"queued" jsonschema:"branches waiting in the merge train"`
	Landed   int `json:"landed"`
}

// TaskLine is one live task.
type TaskLine struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Train  string `json:"train,omitempty"`
	PR     string `json:"pr,omitempty"`
}

// Kinds of needs-you item.
const (
	KindPrompt = "prompt" // an agent waits on a permission prompt or question
	KindNotice = "notice" // an action notice queued for the orchestrator
)

// NeedsYouItem is one thing waiting on the owner. Text, question and
// options come from agents, so they are scrubbed, clipped and untrusted.
type NeedsYouItem struct {
	ID    string `json:"id" jsonschema:"stable while the item is unchanged; a prompt's id changes when its screen does, so an answer bound to it can't land on a newer prompt"`
	Task  string `json:"task"`
	Title string `json:"title,omitempty"`
	Kind  string `json:"kind" jsonschema:"prompt (an agent waits at a prompt) or notice (the orchestrator has an action notice)"`
	Text  string `json:"text"`
	// Prompt is the kind of prompt the screen shows (ask or trust), when
	// it could be read; Question and Options are parsed from it.
	Prompt   string    `json:"prompt,omitempty"`
	Question string    `json:"question,omitempty"`
	Options  []Option  `json:"options,omitempty" jsonschema:"the numbered answers the prompt offers"`
	Since    time.Time `json:"since,omitzero"`
}

// maxText caps item text so a pasted log can't flood a phone.
const maxText = 500

func clip(s string) string { return scrubText(s, maxText) }

// Compact turns the full status into a Snapshot.
func Compact(st mcpserver.StatusOut, queued int, automerge string) Snapshot {
	s := Snapshot{Integration: st.Integration, AutoMerge: automerge, StackAtRisk: st.StackAtRisk,
		CIRed: st.CIRed, Warnings: st.Warnings, Context: st.OrchestratorContext, Tasks: []TaskLine{}}
	s.Counts.Queued = queued
	for _, t := range st.Tasks {
		if t.ID == app.OrchestratorID {
			continue
		}
		switch t.Status {
		case store.Running, store.Idle:
			s.Counts.Running++
		case store.NeedsYou:
			s.Counts.NeedsYou++
		case store.Landed:
			s.Counts.Landed++
			continue
		case store.Killed:
			continue
		}
		s.Tasks = append(s.Tasks, TaskLine{ID: t.ID, Title: t.Title, Status: t.Status, Train: t.Train, PR: t.PR})
	}
	slices.SortStableFunc(s.Tasks, func(a, b TaskLine) int {
		return cmp.Compare(btoi(a.Status != store.NeedsYou), btoi(b.Status != store.NeedsYou))
	})
	return s
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// eventWindow is how far back NeedsYouFrom looks for a task's prompt text.
const eventWindow = 2000

// NeedsYouFrom lists what waits on the owner: agents at a prompt, with the
// prompt's text and, when screen can read the agent's terminal, its
// question and options; then the orchestrator's undelivered action
// notices. It only reads; nothing is marked delivered. screen may be nil,
// and returns "" for a task it can't read.
func NeedsYouFrom(st *store.Store, screen func(store.Task) string) ([]NeedsYouItem, error) {
	ts, err := st.Tasks()
	if err != nil {
		return nil, err
	}
	evs, err := st.Events(eventWindow)
	if err != nil {
		return nil, err
	}
	last := map[string]store.Event{}
	for _, e := range evs {
		if e.Kind == "notification" {
			last[e.Task] = e
		}
	}
	var out []NeedsYouItem
	for _, t := range ts {
		if t.Status != store.NeedsYou || t.ID == app.OrchestratorID {
			continue
		}
		it := NeedsYouItem{Task: t.ID, Title: t.Title, Kind: KindPrompt, Text: "waiting at a prompt"}
		basis := "no notification"
		if e, ok := last[t.ID]; ok {
			it.Text, it.Since = clip(e.Data), e.TS
			basis = fmt.Sprintf("notification %d", e.ID)
		}
		if screen != nil {
			if p := ParsePrompt(screen(t)); p.Kind != app.PromptNone {
				it.Prompt, it.Question, it.Options = p.Kind, p.Question, p.Options
				basis = p.block
			}
		}
		it.ID = promptID(t.ID, basis)
		out = append(out, it)
	}
	ns, err := st.PeekNotices(app.OrchestratorID, true)
	if err != nil {
		return nil, err
	}
	for _, n := range ns {
		out = append(out, NeedsYouItem{ID: noticeID(n.ID), Task: app.OrchestratorID, Kind: KindNotice, Text: clip(n.Text)})
	}
	return out, nil
}

// AppSource reads a repo's saddle state.
type AppSource struct{ A *app.App }

func (s AppSource) Snapshot() (Snapshot, error) {
	st, err := mcpserver.Status(s.A)
	if err != nil {
		return Snapshot{}, err
	}
	q, err := s.A.Queue()
	if err != nil {
		return Snapshot{}, err
	}
	am := "off"
	if m, err := s.A.AutomergeState(); err == nil {
		switch {
		case m.Stopped != "":
			am = "stopped"
		case m.Enabled:
			am = "on"
		}
	}
	snap := Compact(st, len(q), am)
	if e, err := s.A.Limits(time.Now()); err == nil {
		snap.Limits = LimitsFrom(e)
	}
	train, err := s.A.Store.Train()
	if err != nil {
		return Snapshot{}, err
	}
	custom, _ := s.A.CustomStacks()
	risk := ""
	if st.StackAtRisk != nil && !st.StackAtRisk.Acked {
		risk = st.StackAtRisk.Task
	}
	snap.Stacks = StackGraph(s.A.Cfg.Base, train, st.Tasks, custom, risk, st.CIRed)
	return snap, nil
}

func (s AppSource) NeedsYou() ([]NeedsYouItem, error) { return NeedsYouFrom(s.A.Store, s.screen) }

// screen reads a task's terminal for its prompt; "" when it has none.
func (s AppSource) screen(t store.Task) string {
	out, err := s.A.Peek(t.ID, 40)
	if err != nil {
		return ""
	}
	return out
}

func (s AppSource) Peek(task string, lines int) (string, error) { return s.A.Peek(task, lines) }
