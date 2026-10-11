package remote

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/store"
)

// Actor changes a repo's saddle state for the remote action tools (#313).
// AppSource implements it; a Source that doesn't serves only reads.
type Actor interface {
	// State reads the mutable objects actions are versioned against.
	State() (State, error)
	// Screen is a task's terminal, styled when it can be, for safe-send.
	Screen(task string) (string, error)
	// Call runs one of the orchestrator's own MCP tools, so a remote
	// action behaves exactly as the orchestrator's does.
	Call(ctx context.Context, tool string, args map[string]any) (string, error)
}

// State is what remote actions are checked against: each mutable object
// has a version (Versions) that status returns and actions take as
// if_version.
type State struct {
	Queue       []QueueEntry      `json:"queue"`
	AutoMerge   AutoMergeState    `json:"automerge"`
	Concurrency ConcurrencyState  `json:"concurrency"`
	Stack       StackState        `json:"stack"`
	Train       map[string]string `json:"-"` // every task's train state, for unstack and requeue no-ops
}

// QueueEntry is a branch waiting in the merge train, in landing order.
type QueueEntry struct {
	Task string `json:"task"`
	Held bool   `json:"held,omitempty"`
}

// AutoMergeState is the auto-merge toggle and its holds.
type AutoMergeState struct {
	On      bool     `json:"on"`
	Stopped string   `json:"stopped,omitempty"`
	Holds   []string `json:"holds,omitempty"`
}

// ConcurrencyState is the bots limit; Runtime means it overrides config.
type ConcurrencyState struct {
	Limit   int  `json:"limit"`
	Runtime bool `json:"runtime,omitempty"`
}

// StackState is the landed PR stack and what holds it.
type StackState struct {
	Layers []StackLayer  `json:"layers,omitempty"`
	Flag   *FlagState    `json:"flag,omitempty"`
	CIRed  []CIRedLayerV `json:"ci_red,omitempty"`
}

// StackLayer is one landed task in the PR stack.
type StackLayer struct {
	Task string `json:"task"`
	PR   string `json:"pr,omitempty"`
}

// FlagState is the stack sentinel's at-risk flag.
type FlagState struct {
	Task  string `json:"task"`
	Cause string `json:"cause"`
	Acked bool   `json:"acked,omitempty"`
}

// CIRedLayerV is a red layer as far as versions care.
type CIRedLayerV struct {
	Task  string `json:"task"`
	Head  string `json:"head"`
	Acked bool   `json:"acked,omitempty"`
}

// Version objects.
const (
	ObjQueue       = "queue"
	ObjAutoMerge   = "automerge"
	ObjConcurrency = "concurrency"
	ObjStack       = "stack"
)

// objects are the versioned parts of s, by name.
func (s State) objects() map[string]any {
	return map[string]any{ObjQueue: s.Queue, ObjAutoMerge: s.AutoMerge, ObjConcurrency: s.Concurrency, ObjStack: s.Stack}
}

// Versions are a short hash of each mutable object: equal while it is
// unchanged, different once anything in it moved.
func (s State) Versions() map[string]string {
	out := map[string]string{}
	for k, v := range s.objects() {
		out[k] = versionOf(v)
	}
	return out
}

func versionOf(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:6])
}

// describe is one object as JSON, for a refusal that shows what it is now.
func (s State) describe(obj string) string {
	b, _ := json.Marshal(s.objects()[obj])
	return string(b)
}

// State reads the repo's mutable objects.
func (s AppSource) State() (State, error) {
	a := s.A
	st := State{Train: map[string]string{}}
	tasks, err := a.Store.Tasks()
	if err != nil {
		return st, err
	}
	pr := map[string]string{}
	for _, t := range tasks {
		pr[t.ID] = t.PR
	}
	train, err := a.Store.Train()
	if err != nil {
		return st, err
	}
	for _, e := range train {
		st.Train[e.Task] = e.State
		switch e.State {
		case store.Queued, store.OnHold:
			st.Queue = append(st.Queue, QueueEntry{Task: e.Task, Held: e.State == store.OnHold})
		case store.TrainOK:
			st.Stack.Layers = append(st.Stack.Layers, StackLayer{Task: e.Task, PR: pr[e.Task]})
		}
	}
	if m, err := a.AutomergeState(); err == nil {
		st.AutoMerge = AutoMergeState{On: m.Enabled, Stopped: m.Stopped, Holds: m.Holds}
	} else {
		return st, err
	}
	c, err := a.Concurrency()
	if err != nil {
		return st, err
	}
	st.Concurrency = ConcurrencyState{Limit: c.Limit, Runtime: c.Source == app.ConcurrencyRuntime}
	if f, ok, err := a.Flag(); err != nil {
		return st, err
	} else if ok {
		st.Stack.Flag = &FlagState{Task: f.Task, Cause: f.Cause, Acked: f.Acked}
	}
	red, err := a.CIRed()
	if err != nil {
		return st, err
	}
	for _, l := range red.Red {
		st.Stack.CIRed = append(st.Stack.CIRed, CIRedLayerV{Task: l.Task, Head: l.Head, Acked: l.Acked})
	}
	return st, nil
}

// styledCapturer is a tmux driver that can keep SGR escapes, which tell
// the owner's draft from Claude Code's grey suggestion.
type styledCapturer interface {
	CaptureStyled(id string, lines int) (string, error)
}

// Screen is the bottom of a task's terminal, styled when the driver can.
func (s AppSource) Screen(task string) (string, error) {
	t, err := s.A.Store.Task(task)
	if err != nil {
		return "", err
	}
	if t.Window == "" || !s.A.Tmux.Alive(t.Window) {
		return "", fmt.Errorf("%s has no live window (status %s)", task, t.Status)
	}
	if c, ok := s.A.Tmux.(styledCapturer); ok {
		return c.CaptureStyled(t.Window, 30)
	}
	return s.A.Tmux.Capture(t.Window, 30)
}

// Call runs the orchestrator's own tool in process, over an in-memory
// transport, and returns its text.
func (s AppSource) Call(ctx context.Context, tool string, args map[string]any) (string, error) {
	st, ct := mcp.NewInMemoryTransports()
	ss, err := mcpserver.New(s.A, app.OrchestratorID).Connect(ctx, st, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = ss.Close() }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "saddle-remote"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = cs.Close() }()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	if res.IsError {
		return "", errors.New(b.String())
	}
	return b.String(), nil
}
