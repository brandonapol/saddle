package planner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// wireTask is one task as the planner model writes it. Field names follow the
// structured output schema; Parse maps them onto Task.
type wireTask struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Goal     string   `json:"goal"`
	Claims   []string `json:"claims"`
	Deps     []string `json:"deps,omitempty"`
	Model    string   `json:"model,omitempty"`
	Adapter  string   `json:"adapter,omitempty"`
	Barrier  bool     `json:"barrier,omitempty"`
	DoneWhen []string `json:"done_when"`
}

type wirePlan struct {
	Tasks []wireTask `json:"tasks"`
}

func toWire(ts []Task) wirePlan {
	var w wirePlan
	for _, t := range ts {
		w.Tasks = append(w.Tasks, wireTask{
			ID: t.ID, Title: t.Title, Goal: t.Plan, Claims: t.Claims, Deps: t.After,
			Model: t.Model, Adapter: t.Adapter, Barrier: t.Barrier, DoneWhen: t.DoneWhen,
		})
	}
	return w
}

func nonEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

const schemaJSON = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["tasks"],
  "properties": {
    "tasks": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["id", "title", "goal", "claims", "deps", "barrier", "done_when"],
        "properties": {
          "id": {"type": "string", "pattern": "^[a-z0-9][a-z0-9-]*$", "description": "Short kebab-case task id, unique in the plan."},
          "title": {"type": "string", "description": "One-line imperative title."},
          "goal": {"type": "string", "description": "What the task must achieve and how, in a few sentences. This becomes the agent's brief."},
          "claims": {"type": "array", "minItems": 1, "items": {"type": "string"}, "description": "Repo-relative path globs the task may write, e.g. internal/store/** or cmd/saddle/main.go."},
          "deps": {"type": "array", "items": {"type": "string"}, "description": "Ids of tasks that must land before this one starts."},
          "model": {"type": "string", "description": "Optional model for the agent; empty for the default."},
          "adapter": {"type": "string", "description": "Optional agent adapter; empty for the default (claude)."},
          "barrier": {"type": "boolean", "description": "True when the task moves, renames or restructures paths other tasks touch; it runs alone."},
          "done_when": {"type": "array", "minItems": 1, "items": {"type": "string"}, "description": "Concrete, checkable conditions that say the task is finished."}
        }
      }
    }
  }
}`

// Schema returns the JSON schema of the planner's structured output.
func Schema() json.RawMessage { return json.RawMessage(schemaJSON) }

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Parse decodes and strictly validates the planner's JSON output. Unknown
// fields are errors, and every validation problem is reported, not just the
// first.
func Parse(raw []byte) ([]Task, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var w wirePlan
	if err := dec.Decode(&w); err != nil {
		return nil, fmt.Errorf("parse plan: %w", err)
	}
	tasks := make([]Task, 0, len(w.Tasks))
	for _, t := range w.Tasks {
		tasks = append(tasks, Task{
			ID: strings.TrimSpace(t.ID), Title: strings.TrimSpace(t.Title), Plan: strings.TrimSpace(t.Goal),
			Claims: nonEmpty(t.Claims), After: nonEmpty(t.Deps), Model: t.Model, Adapter: t.Adapter,
			Barrier: t.Barrier, DoneWhen: nonEmpty(t.DoneWhen),
		})
	}
	if err := Validate(tasks); err != nil {
		return nil, err
	}
	return tasks, nil
}

// Validate checks that a plan is well formed: every task has an id, title,
// goal, repo-relative claims and done_when checks, ids are unique, and deps
// name other tasks in the plan. It reports every problem it finds.
func Validate(tasks []Task) error {
	if len(tasks) == 0 {
		return errors.New("plan: no tasks")
	}
	var errs []error
	bad := func(i int, t Task, format string, args ...any) {
		name := fmt.Sprintf("task %q", t.ID)
		if t.ID == "" {
			name = fmt.Sprintf("task %d", i)
		}
		errs = append(errs, fmt.Errorf("%s: %s", name, fmt.Sprintf(format, args...)))
	}
	seen := map[string]bool{}
	for _, t := range tasks {
		seen[t.ID] = true
	}
	dup := map[string]bool{}
	for i, t := range tasks {
		switch {
		case t.ID == "":
			bad(i, t, "id is required")
		case !idPattern.MatchString(t.ID):
			bad(i, t, "id must be lowercase letters, digits and dashes")
		case dup[t.ID]:
			errs = append(errs, fmt.Errorf("duplicate task id %q", t.ID))
		}
		dup[t.ID] = true
		if t.Title == "" {
			bad(i, t, "title is required")
		}
		if t.Plan == "" {
			bad(i, t, "goal is required")
		}
		if len(t.Claims) == 0 {
			bad(i, t, "claims must list at least one path glob")
		}
		for _, c := range t.Claims {
			if !relative(c) {
				bad(i, t, "claim %q must be repo-relative", c)
			}
		}
		if len(t.DoneWhen) == 0 {
			bad(i, t, "done_when must list at least one check")
		}
		for _, d := range t.After {
			switch {
			case d == t.ID:
				bad(i, t, "depends on itself")
			case !seen[d]:
				bad(i, t, "depends on unknown task %q", d)
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("plan is invalid:\n%w", errors.Join(errs...))
	}
	return nil
}

func relative(glob string) bool {
	g := strings.TrimSpace(glob)
	if g == "" || strings.HasPrefix(g, "/") || strings.HasPrefix(g, "~") {
		return false
	}
	c := path.Clean(strings.ReplaceAll(g, "\\", "/"))
	return c != ".." && !strings.HasPrefix(c, "../")
}
