// Package hook is the Claude Code hook entrypoint. It enforces claims on
// writes, delivers saddle notices into the session, and tracks agent status.
// It fails open: any internal error lets the agent proceed.
package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/store"
)

type Input struct {
	SessionID      string         `json:"session_id"`
	Event          string         `json:"hook_event_name"`
	Cwd            string         `json:"cwd"`
	ToolName       string         `json:"tool_name"`
	ToolInput      map[string]any `json:"tool_input"`
	Message        string         `json:"message"`
	StopHookActive bool           `json:"stop_hook_active"`
}

type specific struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	AdditionalContext        string `json:"additionalContext,omitempty"`
}

type Output struct {
	Decision string    `json:"decision,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Specific *specific `json:"hookSpecificOutput,omitempty"`
}

// Handle processes one hook invocation for task and returns the JSON to print (nil for none).
func Handle(a *app.App, task string, in Input) *Output {
	st := a.Store
	switch in.Event {
	case "SessionStart":
		_ = st.SetField(task, "session_id", in.SessionID) // fail open
		st.Event(task, "session_start", in.SessionID)
		return context(in.Event, takeAll(st, task))

	case "PreToolUse":
		p := filePath(in.ToolInput)
		if p == "" {
			return nil
		}
		d := a.CheckWrite(task, p)
		if d.Allow {
			return nil
		}
		return &Output{Specific: &specific{HookEventName: "PreToolUse", PermissionDecision: "deny",
			PermissionDecisionReason: "[saddle] " + d.Reason}}

	case "PostToolUse":
		markActive(st, task) // a tool ran, so any prompt it was waiting on was answered
		st.Event(task, "tool", strings.TrimSpace(in.ToolName+" "+filePath(in.ToolInput)))
		return context(in.Event, takeAll(st, task))

	case "UserPromptSubmit":
		markActive(st, task)
		return context(in.Event, takeAll(st, task))

	case "Notification":
		status := store.Idle
		if strings.Contains(strings.ToLower(in.Message), "permission") {
			status = store.NeedsYou
		}
		setIfLive(st, task, status)
		st.Event(task, "notification", in.Message)
		return nil

	case "Stop":
		ns, _ := st.TakeNotices(task, true)
		if len(ns) > 0 {
			markActive(st, task)
			return &Output{Decision: "block", Reason: store.FormatNotices(ns)}
		}
		setIfLive(st, task, store.Idle)
		st.Event(task, "stop", "")
		return nil
	}
	return nil
}

// Run reads hook JSON from r and writes the response to w.
func Run(a *app.App, task string, r io.Reader, w io.Writer) error {
	var in Input
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		return err
	}
	out := Handle(a, task, in)
	if out == nil {
		return nil
	}
	return json.NewEncoder(w).Encode(out)
}

func filePath(in map[string]any) string {
	for _, k := range []string{"file_path", "notebook_path"} {
		if s, ok := in[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func takeAll(st *store.Store, task string) string {
	ns, _ := st.TakeNotices(task, false)
	return store.FormatNotices(ns)
}

func context(event, text string) *Output {
	if text == "" {
		return nil
	}
	return &Output{Specific: &specific{HookEventName: event, AdditionalContext: text}}
}

// markActive flips an idle or waiting agent back to running. Done and
// conflict states belong to the train and are left alone.
func markActive(st *store.Store, task string) {
	t, err := st.Task(task)
	if err == nil && (t.Status == store.Idle || t.Status == store.NeedsYou) {
		_ = st.SetStatus(task, store.Running) // fail open
	}
}

func setIfLive(st *store.Store, task, status string) {
	t, err := st.Task(task)
	if err == nil && (t.Status == store.Running || t.Status == store.Idle || t.Status == store.NeedsYou) {
		_ = st.SetStatus(task, status) // fail open
	}
}

// Describe renders a decision for debugging (`saddle check`).
func Describe(d app.Decision) string {
	if d.Allow {
		return "allow"
	}
	return fmt.Sprintf("deny: %s", d.Reason)
}
