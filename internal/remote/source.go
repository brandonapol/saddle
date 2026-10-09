package remote

import (
	"cmp"
	"slices"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/store"
)

// Source is the state the remote server reads. AppSource reads a repo;
// tests use a fake.
type Source interface {
	Snapshot() (Snapshot, error)
	NeedsYou() ([]NeedsYouItem, error)
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

// NeedsYouItem is one thing waiting on the owner.
type NeedsYouItem struct {
	Task  string    `json:"task"`
	Title string    `json:"title,omitempty"`
	Kind  string    `json:"kind" jsonschema:"prompt (an agent waits at a prompt) or notice (the orchestrator has an action notice)"`
	Text  string    `json:"text"`
	Since time.Time `json:"since,omitzero"`
}

// maxText caps item text so a pasted log can't flood a phone.
const maxText = 500

func clip(s string) string {
	if r := []rune(s); len(r) > maxText {
		return string(r[:maxText-1]) + "…"
	}
	return s
}

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
// prompt's text, then the orchestrator's undelivered action notices. It
// only reads; nothing is marked delivered.
func NeedsYouFrom(st *store.Store) ([]NeedsYouItem, error) {
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
		if e, ok := last[t.ID]; ok {
			it.Text, it.Since = clip(e.Data), e.TS
		}
		out = append(out, it)
	}
	ns, err := st.PeekNotices(app.OrchestratorID, true)
	if err != nil {
		return nil, err
	}
	for _, n := range ns {
		out = append(out, NeedsYouItem{Task: app.OrchestratorID, Kind: KindNotice, Text: clip(n.Text)})
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
	return Compact(st, len(q), am), nil
}

func (s AppSource) NeedsYou() ([]NeedsYouItem, error) { return NeedsYouFrom(s.A.Store) }
