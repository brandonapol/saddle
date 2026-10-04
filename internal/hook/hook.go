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
	// Grok is set when the payload came from the Grok CLI (camelCase keys).
	Grok bool `json:"-"`
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
		if cmd, _ := in.ToolInput["command"].(string); cmd != "" {
			if why := BypassesHooks(cmd); why != "" {
				st.Event(task, "no_verify_denied", cmd)
				return deny(in, why+". The repo's gate must pass: fix what it reports (run its fixer if it has one), then commit normally. done runs the same check.")
			}
		}
		p := filePath(in.ToolInput)
		if p == "" {
			return nil
		}
		d := a.CheckWrite(task, p)
		if d.Allow {
			return nil
		}
		return deny(in, d.Reason)

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

// deny refuses a tool call with reason.
func deny(in Input, reason string) *Output {
	out := &Output{Specific: &specific{HookEventName: "PreToolUse", PermissionDecision: "deny",
		PermissionDecisionReason: "[saddle] " + reason}}
	// Grok reads the top-level decision. Claude's only takes approve or
	// block, so it gets hookSpecificOutput alone.
	if in.Grok {
		out.Decision, out.Reason = "deny", "[saddle] "+reason
	}
	return out
}

// HandleOrchestrator processes one hook invocation from the user's own Claude
// Code session orchestrating through the saddle plugin. It only delivers the
// orchestrator's notices: the session writes no claimed files, and its
// session id is not recorded, so the TUI never resumes the user's session.
func HandleOrchestrator(a *app.App, in Input) *Output {
	st := a.Store
	switch in.Event {
	case "SessionStart", "UserPromptSubmit", "PostToolUse":
		return context(in.Event, takeAll(st, app.OrchestratorID))
	case "Stop":
		if in.StopHookActive {
			return nil // it already continued once for notices; let it stop
		}
		ns, _ := st.TakeNotices(app.OrchestratorID, true)
		if len(ns) > 0 {
			return &Output{Decision: "block", Reason: store.FormatNotices(ns)}
		}
	}
	return nil
}

// Run reads hook JSON from r and writes the response to w.
// Claude Code sends snake_case fields; the Grok CLI sends camelCase (and
// sometimes both). Either shape is accepted.
func Run(a *app.App, task string, r io.Reader, w io.Writer) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	in, err := decodeInput(b)
	if err != nil {
		return err
	}
	out := Handle(a, task, in)
	if out == nil {
		return nil
	}
	return json.NewEncoder(w).Encode(out)
}

type inputWire struct {
	SessionIDSnake string         `json:"session_id"`
	SessionIDCamel string         `json:"sessionId"`
	EventSnake     string         `json:"hook_event_name"`
	EventCamel     string         `json:"hookEventName"`
	ToolSnake      string         `json:"tool_name"`
	ToolCamel      string         `json:"toolName"`
	InputSnake     map[string]any `json:"tool_input"`
	InputCamel     map[string]any `json:"toolInput"`
	Message        string         `json:"message"`
	NoticeType     string         `json:"notificationType"`
	StopSnake      bool           `json:"stop_hook_active"`
	StopCamel      bool           `json:"stopHookActive"`
}

func decodeInput(b []byte) (Input, error) {
	var w inputWire
	if err := json.Unmarshal(b, &w); err != nil {
		return Input{}, err
	}
	in := Input{
		SessionID:      first(w.SessionIDSnake, w.SessionIDCamel),
		Event:          hookEvent(w.EventSnake, w.EventCamel),
		ToolName:       first(w.ToolSnake, w.ToolCamel),
		ToolInput:      w.InputSnake,
		Message:        w.Message,
		StopHookActive: w.StopSnake || w.StopCamel,
		Grok:           w.EventCamel != "" || w.SessionIDCamel != "" || w.ToolCamel != "",
	}
	if in.ToolInput == nil {
		in.ToolInput = w.InputCamel
	}
	if in.Message == "" {
		in.Message = w.NoticeType
	}
	return in, nil
}

func first(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// hookEvent prefers Claude's PascalCase name. Grok sends that in
// hook_event_name and a snake_case alias in hookEventName.
func hookEvent(snake, camel string) string {
	if looksPascal(snake) {
		return snake
	}
	if looksPascal(camel) {
		return camel
	}
	if ev, ok := eventAlias[snake]; ok {
		return ev
	}
	if ev, ok := eventAlias[camel]; ok {
		return ev
	}
	return first(snake, camel)
}

func looksPascal(s string) bool {
	return s != "" && s[0] >= 'A' && s[0] <= 'Z'
}

var eventAlias = map[string]string{
	"session_start":      "SessionStart",
	"pre_tool_use":       "PreToolUse",
	"post_tool_use":      "PostToolUse",
	"user_prompt_submit": "UserPromptSubmit",
	"notification":       "Notification",
	"stop":               "Stop",
}

func filePath(in map[string]any) string {
	if in == nil {
		return ""
	}
	for _, k := range []string{"file_path", "filePath", "path", "target_file", "notebook_path"} {
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
