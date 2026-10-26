package mcpserver

import (
	"errors"
	"fmt"
	"strings"

	"github.com/brandonapol/saddle/internal/app"
)

// AfterIn drives the after tool (#675).
type AfterIn struct {
	Task  string   `json:"task,omitempty" jsonschema:"the task that builds on the others; yours by default (only the orchestrator may name another)"`
	After []string `json:"after" jsonschema:"the task ids whose work it builds on"`
}

const afterDesc = "Record after edges once a task is spawned: it builds on these tasks' work, so prs stacks its PR on theirs even when files don't overlap and no issue is shared. " +
	"Use it when your work needs a sibling's unmerged commit, or when the pre-publish gate says a layer fails for want of one (it prints the exact edge). Never edit .saddle/deps by hand."

func afterTool(a *app.App, caller string, in AfterIn) (OK, error) {
	task := in.Task
	if task == "" {
		task = caller
	}
	if task == "" {
		return OK{}, errors.New("SADDLE_TASK is not set; name the task")
	}
	if task != caller && caller != app.OrchestratorID {
		return OK{}, fmt.Errorf("only the orchestrator can record edges for another task (%s)", task)
	}
	added, err := a.After(task, in.After)
	if err != nil {
		return OK{}, err
	}
	if len(added) == 0 {
		return OK{Message: task + " already runs after " + strings.Join(in.After, ", ")}, nil
	}
	return OK{Message: fmt.Sprintf("%s now runs after %s; prs stacks it on them.", task, strings.Join(added, ", "))}, nil
}
