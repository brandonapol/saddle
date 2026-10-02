package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Snapshot is what the planner model sees of the repo.
type Snapshot struct {
	Tree       []string // tracked paths, collapsed below a depth limit
	Codeowners string   // CODEOWNERS contents, if any
	Churn      []Churn  // most-changed files recently, busiest first
	Serial     []string // config serial globs: only the merge train writes these
}

// Churn counts recent commits touching a path.
type Churn struct {
	Path    string
	Commits int
}

// Request is one planning run. Previous and Note make it a re-plan: the
// model revises Previous as the note asks.
type Request struct {
	Epic     string
	Snapshot Snapshot
	Previous []Task
	Note     string
}

// Call is everything a Model needs for one structured completion.
type Call struct {
	System string
	User   string
	Schema json.RawMessage
}

// Model turns a Call into JSON that should match Call.Schema. It is the only
// part of planning that talks to an LLM, so tests can substitute a fake.
type Model interface {
	Complete(ctx context.Context, c Call) ([]byte, error)
}

// Draft is a validated plan and the checker's schedule for it.
type Draft struct {
	Tasks []Task
	Check Plan
}

const systemPrompt = `You are the planner for saddle, which runs several coding agents in parallel, one git worktree and branch per task, and lands their branches one at a time through a merge train.

Split the epic into tasks. Rules:
- Each task is a coherent change one agent can finish and test alone, usually a few hours of work. Prefer fewer, larger tasks to many tiny ones.
- claims are repo-relative path globs (doublestar syntax, e.g. internal/store/** or cmd/saddle/main.go) that the task may write. An agent cannot write outside its claims. Claim what the task needs, no more: overlapping claims serialize tasks.
- Tasks whose claims overlap run one after another; disjoint tasks run in parallel. Keep claims disjoint where you can.
- Serial globs (lockfiles, generated files, migrations) are written only through the merge train. Avoid claiming them unless the task must change them.
- deps lists ids of tasks that must land first because this task builds on their code.
- Set barrier to true only for tasks that move, rename or restructure paths other tasks touch; a barrier runs alone.
- goal is the agent's brief: what to build, where, and how it fits the repo.
- done_when lists concrete checks (tests that pass, commands that work).
- Leave model and adapter empty unless the epic asks for a specific one.`

// BuildCall assembles the planner prompt for req.
func BuildCall(req Request) Call {
	var b strings.Builder
	fmt.Fprintf(&b, "# Epic\n\n%s\n", strings.TrimSpace(req.Epic))
	s := req.Snapshot
	if len(s.Tree) > 0 {
		fmt.Fprintf(&b, "\n# Repo tree\n\n%s\n", strings.Join(s.Tree, "\n"))
	}
	if c := strings.TrimSpace(s.Codeowners); c != "" {
		fmt.Fprintf(&b, "\n# CODEOWNERS\n\n%s\n", c)
	}
	if len(s.Churn) > 0 {
		b.WriteString("\n# Recent churn\n\n")
		for _, c := range s.Churn {
			fmt.Fprintf(&b, "%s (%d commits)\n", c.Path, c.Commits)
		}
	}
	if len(s.Serial) > 0 {
		fmt.Fprintf(&b, "\n# Serial globs\n\n%s\n", strings.Join(s.Serial, "\n"))
	}
	if len(req.Previous) > 0 {
		prev, _ := json.MarshalIndent(toWire(req.Previous), "", "  ")
		fmt.Fprintf(&b, "\n# Previous plan\n\n%s\n", prev)
	}
	if n := strings.TrimSpace(req.Note); n != "" {
		fmt.Fprintf(&b, "\n# Revise the previous plan as the user asks\n\n%s\n", n)
	}
	b.WriteString("\nSubmit the plan as JSON matching the schema.\n")
	return Call{System: systemPrompt, User: b.String(), Schema: Schema()}
}

// Generate asks m for a plan, validates it strictly and runs Check with the
// snapshot's serial globs and the wave limit. An invalid plan gets one retry
// that shows the model what was wrong.
func Generate(ctx context.Context, m Model, req Request, limit int) (Draft, error) {
	c := BuildCall(req)
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		if last != nil {
			c.User += fmt.Sprintf("\n# Your previous answer was rejected\n\n%v\n\nFix every problem and submit the whole plan again.\n", last)
		}
		raw, err := m.Complete(ctx, c)
		if err != nil {
			return Draft{}, err
		}
		d, err := draft(raw, req.Snapshot.Serial, limit)
		if err == nil {
			return d, nil
		}
		last = err
	}
	return Draft{}, last
}

func draft(raw []byte, serial []string, limit int) (Draft, error) {
	tasks, err := Parse(raw)
	if err != nil {
		return Draft{}, err
	}
	p, err := Check(tasks, serial, limit)
	if err != nil {
		return Draft{}, err
	}
	return Draft{Tasks: tasks, Check: p}, nil
}
