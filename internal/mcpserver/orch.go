package mcpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// StatusIn narrows the status tool (#340). With no fields set it is compact:
// essentials per task, finished tasks counted rather than listed.
type StatusIn struct {
	Detail bool     `json:"detail,omitempty" jsonschema:"full dump: every task with claims, branch, worktree, window and model, finished tasks included; large with many tasks"`
	State  []string `json:"state,omitempty" jsonschema:"only tasks in these states, e.g. [\"running\",\"needs_you\",\"conflict\"]; a train state such as merged or queued matches too"`
	IDs    []string `json:"ids,omitempty" jsonschema:"only these task ids, e.g. [\"t3\",\"t7\"]"`
	Since  string   `json:"since,omitempty" jsonschema:"only tasks spawned since then: a duration back from now (90m, 2h) or an RFC 3339 time"`
}

// compactClip bounds the free-text fields of a compact task view.
const compactClip = 160

const compactHint = "Compact view: claims, branches, worktrees, windows and models are left out and finished tasks are counted in hidden. " +
	"Pass detail=true for the full dump, state or ids to pick tasks (finished ones included), since for recent ones."

// statusView applies in to a full status: filters, then compacts unless
// in.Detail.
func statusView(out StatusOut, in StatusIn, now time.Time) (StatusOut, error) {
	var since time.Time
	if s := strings.TrimSpace(in.Since); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			since = now.Add(-d)
		} else if ts, err := time.Parse(time.RFC3339, s); err == nil {
			since = ts
		} else {
			return out, fmt.Errorf("since %q: want a duration such as 2h or an RFC 3339 time", in.Since)
		}
	}
	picked := len(in.State) > 0 || len(in.IDs) > 0
	tasks := make([]TaskView, 0, len(out.Tasks))
	for _, v := range out.Tasks {
		switch {
		case len(in.IDs) > 0 && !slices.Contains(in.IDs, v.ID):
			continue
		case len(in.State) > 0 && !slices.Contains(in.State, v.Status) && !slices.Contains(in.State, trainState(v.Train)):
			continue
		case !since.IsZero() && v.created.Before(since):
			continue
		}
		if !in.Detail && !picked {
			if k := finished(v); k != "" {
				if out.Hidden == nil {
					out.Hidden = map[string]int{}
				}
				out.Hidden[k]++
				continue
			}
		}
		if !in.Detail {
			v = compactTask(v)
		}
		tasks = append(tasks, v)
	}
	out.Tasks = tasks
	if !in.Detail {
		out.Hint = compactHint
	}
	return out, nil
}

// finished names why a compact status leaves v out, or "" to list it.
func finished(v TaskView) string {
	switch {
	case v.Status == store.Killed, v.Status == store.NoChange:
		return v.Status
	case trainState(v.Train) == "merged":
		return "merged"
	}
	return ""
}

// trainState is the state word of a TaskView.Train ("landed: a..b" -> "landed").
func trainState(train string) string {
	s, _, _ := strings.Cut(train, ":")
	return s
}

func compactTask(v TaskView) TaskView {
	return TaskView{ID: v.ID, Title: v.Title, Status: v.Status, Reason: clip(v.Reason), Parent: v.Parent,
		Train: clip(v.Train), Notices: v.Notices, PR: v.PR}
}

func clip(s string) string {
	if len(s) <= compactClip {
		return s
	}
	return s[:compactClip] + "…"
}

// landMessage says what land did, so an empty run is never a bare null (#340).
func landMessage(n int) string {
	if n == 0 {
		return "nothing queued: no branch is waiting in the merge train. A task is queued when it calls done; " +
			"status shows which tasks are still running or idle without having called done."
	}
	return fmt.Sprintf("Ran the merge train over %d queued branch(es); see results.", n)
}

// KeyList is send_keys' keys: a JSON array of key names, or one key as a
// plain string ("2") (#340).
type KeyList []string

func (k *KeyList) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*k = KeyList{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New("keys: want a key name or a list of key names")
	}
	*k = many
	return nil
}

// keysSchema is send_keys' input schema. It is written out because the
// inferred one only allows an array for keys.
var keysSchema = json.RawMessage(`{
	"type": "object",
	"required": ["task"],
	"additionalProperties": false,
	"properties": {
		"task": {"type": "string", "description": "task id"},
		"text": {"type": "string", "description": "text to type followed by Enter, e.g. an answer to the agent's question"},
		"keys": {
			"description": "tmux key names to press instead: one key as a string (\"1\" to choose option 1 of a prompt, \"Escape\") or a list ([\"Down\",\"Enter\"])",
			"anyOf": [
				{"type": "string"},
				{"type": "array", "items": {"type": "string"}}
			]
		}
	}
}`)
