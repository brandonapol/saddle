// Package mcpserver is the stdio MCP server each agent gets. The calling task
// comes from SADDLE_TASK, so spawn sets the parent and claim/done apply to it.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type SpawnIn struct {
	Title  string   `json:"title" jsonschema:"short task title, e.g. 'meter ingestion worker'"`
	Prompt string   `json:"prompt" jsonschema:"self-contained instructions: goal, files, constraints, how to verify, done-when"`
	Claims []string `json:"claims,omitempty" jsonschema:"repo-relative path globs this task owns, e.g. internal/meter/**; must not overlap other live tasks"`
	Model  string   `json:"model,omitempty" jsonschema:"claude model alias: opus, sonnet or haiku; defaults to config"`
	ID     string   `json:"id,omitempty" jsonschema:"optional task id; defaults to the next tN"`
	Issue  int      `json:"issue,omitempty" jsonschema:"GitHub issue number this task implements; its PR will close it"`
	// Confirm overrides app.ErrNeedsConfirm.
	Confirm bool `json:"confirm,omitempty" jsonschema:"spawn even though every claim covers work landed or queued tasks already did; only after the user agreed"`
}

type PeekIn struct {
	Task  string `json:"task" jsonschema:"task id, e.g. t3"`
	Lines int    `json:"lines,omitempty" jsonschema:"how many lines of the agent's terminal to return (default 40)"`
}

type PeekOut struct {
	Screen string `json:"screen"`
}

type KeysIn struct {
	Task string   `json:"task" jsonschema:"task id"`
	Text string   `json:"text,omitempty" jsonschema:"text to type followed by Enter, e.g. an answer to the agent's question"`
	Keys []string `json:"keys,omitempty" jsonschema:"tmux key names to press instead, e.g. [\"1\"] to choose option 1 of a prompt, [\"Escape\"], [\"Down\",\"Enter\"]"`
}

type TicketIn struct {
	Number int `json:"number" jsonschema:"GitHub issue number"`
}

type SpawnOut struct {
	ID     string `json:"id"`
	Branch string `json:"branch"`
	Window string `json:"window"`
}

type PathsIn struct {
	Paths []string `json:"paths" jsonschema:"repo-relative paths or globs"`
}

type DoneIn struct {
	Summary string `json:"summary" jsonschema:"2-4 sentences: what changed and why; used as the PR description"`
}

type TaskIn struct {
	Task string `json:"task" jsonschema:"task id, e.g. t3"`
}

type MessageIn struct {
	Task    string `json:"task" jsonschema:"task id to message"`
	Message string `json:"message"`
}

type TaskView struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Status   string   `json:"status"`
	Reason   string   `json:"reason,omitempty"` // why a spawn failed
	Model    string   `json:"model,omitempty"`
	Parent   string   `json:"parent,omitempty"`
	Branch   string   `json:"branch,omitempty"`
	Claims   []string `json:"claims,omitempty"`
	Train    string   `json:"train,omitempty"`
	Notices  int      `json:"pending_notices,omitempty"`
	PR       string   `json:"pr,omitempty"`
	Window   string   `json:"window,omitempty"`
	Worktree string   `json:"worktree,omitempty"`
}

type StatusOut struct {
	Integration string     `json:"integration"`
	Warnings    []string   `json:"warnings,omitempty"`
	Tasks       []TaskView `json:"tasks"`
}

type OK struct {
	Message string `json:"message"`
}

type LandOut struct {
	Results []app.LandResult `json:"results"`
}

// RestackConflict is the landed commit restack stopped at and the task that owns it.
type RestackConflict struct {
	Task   string   `json:"task"`
	Commit string   `json:"commit"`
	Files  []string `json:"files"`
}

type RestackOut struct {
	Message    string            `json:"message"`
	Base       string            `json:"base,omitempty"`
	Moves      []app.RestackMove `json:"moves,omitempty"`
	Skipped    []string          `json:"skipped,omitempty" jsonschema:"tasks whose work base already has; their PRs were left alone"`
	Retargeted []string          `json:"retargeted,omitempty"`
	Dropped    int               `json:"dropped,omitempty"`
	Conflict   *RestackConflict  `json:"conflict,omitempty"`
}

type PRsOut struct {
	PRs []string `json:"prs"`
}

// Status builds the shared status view used by the MCP tool and the CLI.
func Status(a *app.App) (StatusOut, error) {
	out := StatusOut{Integration: a.Cfg.Integration, Warnings: a.Warnings()}
	ts, err := a.Store.Tasks()
	if err != nil {
		return out, err
	}
	cl, err := a.Store.Claims()
	if err != nil {
		return out, err
	}
	train, err := a.Store.Train()
	if err != nil {
		return out, err
	}
	tr := map[string]string{}
	for _, e := range train {
		s := e.State
		if e.Note != "" {
			s += ": " + e.Note
		}
		tr[e.Task] = s
	}
	for _, t := range ts {
		n, _ := a.Store.PendingNotices(t.ID)
		reason := ""
		if t.Status == app.StatusFailed {
			reason = t.Summary
		}
		out.Tasks = append(out.Tasks, TaskView{Reason: reason,
			ID: t.ID, Title: t.Title, Status: t.Status, Model: t.Model, Parent: t.Parent, Branch: t.Branch,
			Claims: cl[t.ID], Train: tr[t.ID], Notices: n, PR: t.PR, Window: t.Window, Worktree: t.Worktree,
		})
	}
	return out, nil
}

func Serve(ctx context.Context, a *app.App, task string) error {
	return New(a, task).Run(ctx, &mcp.StdioTransport{})
}

// New builds the MCP server for task without starting it.
func New(a *app.App, task string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "saddle", Version: "0.1.0"}, &mcp.ServerOptions{
		Instructions: "Saddle coordinates parallel coding agents. Use spawn for disjoint sub-work, claim before large edits, and done when your branch is committed and tested.",
	})
	self := func() error {
		if task == "" {
			return errors.New("SADDLE_TASK is not set; this MCP server must be started by saddle")
		}
		return nil
	}

	mcp.AddTool(s, &mcp.Tool{Name: "spawn", Description: "Start a new parallel agent on its own branch and worktree in a new tmux window. Give it disjoint claims."},
		func(_ context.Context, _ *mcp.CallToolRequest, in SpawnIn) (*mcp.CallToolResult, SpawnOut, error) {
			if err := self(); err != nil {
				return nil, SpawnOut{}, err
			}
			t, err := a.Spawn(app.SpawnReq{ID: in.ID, Title: in.Title, Prompt: in.Prompt, Claims: in.Claims, Model: in.Model, Parent: task, Issue: in.Issue, Confirm: in.Confirm})
			if err != nil {
				return nil, SpawnOut{}, err
			}
			return nil, SpawnOut{ID: t.ID, Branch: t.Branch, Window: t.Window}, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "claim", Description: "Reserve paths or globs for your task. Paths that overlap another live task are denied, naming the owner."},
		func(_ context.Context, _ *mcp.CallToolRequest, in PathsIn) (*mcp.CallToolResult, app.ClaimResult, error) {
			if err := self(); err != nil {
				return nil, app.ClaimResult{}, err
			}
			r, err := a.Claim(task, in.Paths)
			return nil, r, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "release", Description: "Give up claims you no longer need so other tasks can use those paths."},
		func(_ context.Context, _ *mcp.CallToolRequest, in PathsIn) (*mcp.CallToolResult, OK, error) {
			if err := self(); err != nil {
				return nil, OK{}, err
			}
			return nil, OK{Message: "released"}, a.Store.Release(task, in.Paths...)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "status", Description: "List every saddle task with status, claims and merge-train state."},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, StatusOut, error) {
			out, err := Status(a)
			return nil, out, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "done", Description: "Finish your task: requires a clean, committed worktree. Queues your branch in the merge train."},
		func(_ context.Context, _ *mcp.CallToolRequest, in DoneIn) (*mcp.CallToolResult, OK, error) {
			if err := self(); err != nil {
				return nil, OK{}, err
			}
			if err := a.Done(task, in.Summary); err != nil {
				return nil, OK{}, err
			}
			return nil, OK{Message: "Queued in the merge train. You can stop now. If it conflicts or fails tests, you will be told."}, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "message", Description: "Send a [saddle] message to another task. It wakes the agent if it is idle."},
		func(_ context.Context, _ *mcp.CallToolRequest, in MessageIn) (*mcp.CallToolResult, OK, error) {
			from := task
			if from == "" {
				from = "user"
			}
			err := a.Notify(in.Task, store.NoticeAction, fmt.Sprintf("Message from %s: %s", from, in.Message))
			return nil, OK{Message: "sent"}, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "land", Description: "Run the merge train: land queued branches one at a time on the integration branch (rebase, test, fast-forward). Conflicts go back to their agents."},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, LandOut, error) {
			rs, err := a.Land()
			return nil, LandOut{Results: rs}, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "prs", Description: "Push landed branches and open or update a stack of GitHub PRs, in landing order."},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, PRsOut, error) {
			urls, err := a.PRs()
			return nil, PRsOut{PRs: urls}, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "restack", Description: "Rebuild the landed stack on origin's base after the base moved or a bottom PR merged: rebases in train order, moves the branches, pushes and retargets PRs. A conflict moves nothing and goes back to the task that owns the commit."},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, RestackOut, error) {
			res, err := a.Restack()
			var c *app.RestackConflict
			if errors.As(err, &c) {
				return nil, RestackOut{Message: c.Error(), Conflict: &RestackConflict{Task: c.Task, Commit: c.Commit, Files: c.Files}}, nil
			}
			if err != nil {
				return nil, RestackOut{}, err
			}
			return nil, RestackOut{
				Message: fmt.Sprintf("Restacked onto %s: %d refs moved, %d PRs retargeted.", res.Base, len(res.Moves), len(res.Retargeted)),
				Base:    res.Base, Moves: res.Moves, Skipped: res.Merged, Retargeted: res.Retargeted, Dropped: res.Dropped,
			}, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "peek", Description: "Read the last lines of a task's Claude Code terminal, e.g. to see what it is stuck on or what a prompt is asking."},
		func(_ context.Context, _ *mcp.CallToolRequest, in PeekIn) (*mcp.CallToolResult, PeekOut, error) {
			out, err := a.Peek(in.Task, in.Lines)
			return nil, PeekOut{Screen: out}, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "send_keys", Description: "Type into a task's terminal: answer its question (text) or pick a prompt option (keys). Claude Code menus: a numbered menu takes the digit alone (keys [\"1\"]); an unnumbered menu needs arrows then Enter (keys [\"Down\",\"Enter\"]). Only answer when it is clearly safe or the user told you what to answer. Peek first, and peek again afterwards to confirm the prompt is gone."},
		func(_ context.Context, _ *mcp.CallToolRequest, in KeysIn) (*mcp.CallToolResult, OK, error) {
			if in.Text == "" && len(in.Keys) == 0 {
				return nil, OK{}, errors.New("give text or keys")
			}
			return nil, OK{Message: "sent"}, a.SendKeys(in.Task, in.Text, in.Keys)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "ticket", Description: "Fetch a GitHub issue (title, body, labels, sub-issues) from this repo to plan tasks from."},
		func(_ context.Context, _ *mcp.CallToolRequest, in TicketIn) (*mcp.CallToolResult, app.Ticket, error) {
			t, err := a.Ticket(in.Number)
			return nil, t, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "kill", Description: "Stop a task's agent and release its claims. Its worktree is removed; a branch with commits is kept."},
		func(_ context.Context, _ *mcp.CallToolRequest, in TaskIn) (*mcp.CallToolResult, OK, error) {
			return nil, OK{Message: "killed " + strings.TrimSpace(in.Task)}, a.Kill(in.Task, false)
		})

	return s
}
