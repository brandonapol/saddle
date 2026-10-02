// Package mcpserver is the stdio MCP server each agent gets. The calling task
// comes from SADDLE_TASK, so spawn sets the parent and claim/done apply to it.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/sentinel"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type SpawnIn struct {
	Title  string   `json:"title" jsonschema:"short task title, e.g. 'meter ingestion worker'"`
	Prompt string   `json:"prompt" jsonschema:"self-contained instructions: goal, files, constraints, how to verify, done-when"`
	Claims []string `json:"claims,omitempty" jsonschema:"repo-relative path globs this task owns, e.g. internal/meter/**; must not overlap other live tasks"`
	Model  string   `json:"model,omitempty" jsonschema:"model for the adapter, e.g. claude's opus, sonnet or haiku; defaults to config for claude and to the CLI's default otherwise"`
	// Adapter picks the agent CLI; see agent.ByName.
	Adapter string `json:"adapter,omitempty" jsonschema:"agent to run: claude (default), codex (general-purpose) or grok (image generation, e.g. hero art). Codex and Grok have no saddle hooks, so their claims are advisory"`
	ID      string `json:"id,omitempty" jsonschema:"optional task id; defaults to the next tN"`
	Issue   int    `json:"issue,omitempty" jsonschema:"GitHub issue number this task implements; its PR will close it"`
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
	StackAtRisk *StackRisk `json:"stack_at_risk,omitempty"`
	Tasks       []TaskView `json:"tasks"`
}

// StackRisk is the stack sentinel's flag: the stack is at risk from Task up.
type StackRisk struct {
	Task  string   `json:"task"`            // the first broken task
	Cause string   `json:"cause"`           // why it broke
	PRs   []string `json:"prs,omitempty"`   // PRs labeled needs-human
	Acked bool     `json:"acked,omitempty"` // acknowledged: it freezes nothing
	Fix   string   `json:"fix"`
}

// StackFix is what to do about a flagged stack.
const StackFix = "run restack to rebuild the stack; don't fix it with git or a worker. " +
	"prs and land hold back only what depends on the broken layers until it checks clean, then the flag and labels clear by themselves. " +
	"If restack can't fix it, unstack drops a task from the stack and sentinel_ack acknowledges the flag; never edit state.db"

type UnstackIn struct {
	Task string `json:"task" jsonschema:"task id (t3), PR URL, or PR number (#12)"`
}

// BriefOut is a task's live brief: what to do, what it owns and what it must not touch.
type BriefOut struct {
	Task       string              `json:"task"`
	Title      string              `json:"title"`
	Goal       string              `json:"goal,omitempty"`
	Status     string              `json:"status"`
	Adapter    string              `json:"adapter"`
	Branch     string              `json:"branch,omitempty"`
	Worktree   string              `json:"worktree,omitempty"`
	Parent     string              `json:"parent,omitempty"`
	Claims     []string            `json:"claims,omitempty"`
	DoNotTouch map[string][]string `json:"do_not_touch,omitempty" jsonschema:"other live tasks' claims, by task id; ask_owner reaches them"`
	Serial     []string            `json:"serial,omitempty" jsonschema:"files only the merge train changes"`
	Children   []TaskView          `json:"children,omitempty"`
	Notices    int                 `json:"pending_notices,omitempty"`
	DoneWhen   string              `json:"done_when"`
}

// DoneWhen is the finish line every worker brief states.
const DoneWhen = "tests pass and everything is committed; then call done with a 2-4 sentence summary"

type AskIn struct {
	Path     string `json:"path" jsonschema:"repo-relative path you need changed or have a question about"`
	Question string `json:"question"`
}

type AskOut struct {
	Owner   string `json:"owner" jsonschema:"task that was asked; t0 is the orchestrator"`
	Message string `json:"message"`
}

// Brief builds task's live brief from the store.
func Brief(a *app.App, task string) (BriefOut, error) {
	t, err := a.Store.Task(task)
	if err != nil {
		return BriefOut{}, err
	}
	out := BriefOut{Task: t.ID, Title: t.Title, Goal: t.Prompt, Status: t.Status, Adapter: a.AdapterName(t),
		Branch: t.Branch, Worktree: t.Worktree, Parent: t.Parent, Serial: a.Cfg.Serial, DoneWhen: DoneWhen}
	views, err := Tasks(a)
	if err != nil {
		return out, err
	}
	for _, v := range views {
		switch {
		case v.ID == task:
			out.Claims, out.Notices = v.Claims, v.Notices
		case len(v.Claims) > 0:
			if out.DoNotTouch == nil {
				out.DoNotTouch = map[string][]string{}
			}
			out.DoNotTouch[v.ID] = v.Claims
		}
		if v.Parent == task {
			out.Children = append(out.Children, v)
		}
	}
	return out, nil
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
	Superseded []string          `json:"superseded,omitempty" jsonschema:"tasks out of the stack (killed, PR closed, unstacked) whose commits left integration"`
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
	f, flagged, err := a.Flag()
	if err != nil {
		return out, err
	}
	if flagged {
		out.StackAtRisk = &StackRisk{Task: f.Task, Cause: f.Cause, PRs: f.PRs, Acked: f.Acked, Fix: StackFix}
	}
	out.Tasks, err = Tasks(a)
	return out, err
}

// Tasks lists every task with its claims, train state and pending notices.
// It reads only the store, so the TUI can poll it every second.
func Tasks(a *app.App) ([]TaskView, error) {
	ts, err := a.Store.Tasks()
	if err != nil {
		return nil, err
	}
	cl, err := a.Store.Claims()
	if err != nil {
		return nil, err
	}
	train, err := a.Store.Train()
	if err != nil {
		return nil, err
	}
	pending, err := a.Store.PendingNoticeCounts()
	if err != nil {
		return nil, err
	}
	tr := map[string]string{}
	for _, e := range train {
		s := e.State
		if e.Note != "" {
			s += ": " + e.Note
		}
		tr[e.Task] = s
	}
	out := make([]TaskView, 0, len(ts))
	for _, t := range ts {
		reason := ""
		if t.Status == app.StatusFailed {
			reason = t.Summary
		}
		out = append(out, TaskView{Reason: reason,
			ID: t.ID, Title: t.Title, Status: t.Status, Model: t.Model, Parent: t.Parent, Branch: t.Branch,
			Claims: cl[t.ID], Train: tr[t.ID], Notices: pending[t.ID], PR: t.PR, Window: t.Window, Worktree: t.Worktree,
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

	mcp.AddTool(s, &mcp.Tool{Name: "spawn", Description: fmt.Sprintf("Start a new parallel agent on its own branch and worktree in a new tmux window. Give it disjoint claims. Sub-tasks are capped in depth (%d below the orchestrator) and in working children per task (%d).", app.DefaultMaxDepth, app.DefaultMaxChildren)},
		func(_ context.Context, _ *mcp.CallToolRequest, in SpawnIn) (*mcp.CallToolResult, SpawnOut, error) {
			if err := self(); err != nil {
				return nil, SpawnOut{}, err
			}
			t, err := a.Spawn(app.SpawnReq{ID: in.ID, Title: in.Title, Prompt: in.Prompt, Claims: in.Claims, Model: in.Model, Adapter: in.Adapter, Parent: task, Issue: in.Issue, Confirm: in.Confirm})
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

	mcp.AddTool(s, &mcp.Tool{Name: "status", Description: "List every saddle task with status, claims and merge-train state, plus warnings and stack_at_risk when the stack sentinel has flagged the PR stack."},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, StatusOut, error) {
			out, err := Status(a)
			return nil, out, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "brief", Description: "Your task's live brief: goal, branch, your claims, other tasks' claims you must not touch, serial files, your sub-tasks and when you are done."},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, BriefOut, error) {
			if err := self(); err != nil {
				return nil, BriefOut{}, err
			}
			out, err := Brief(a, task)
			return nil, out, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "ask_owner", Description: "Ask whoever owns a path a question, e.g. before you need a change in a file another task claims. It goes to the claiming task, or to the orchestrator for serial files; the answer arrives as a [saddle] message."},
		func(_ context.Context, _ *mcp.CallToolRequest, in AskIn) (*mcp.CallToolResult, AskOut, error) {
			if err := self(); err != nil {
				return nil, AskOut{}, err
			}
			owner, err := a.AskOwner(task, in.Path, in.Question)
			if err != nil {
				return nil, AskOut{}, err
			}
			return nil, AskOut{Owner: owner, Message: "Asked " + owner + "; the answer will arrive as a [saddle] message. Keep working on what you can meanwhile."}, nil
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

	mcp.AddTool(s, &mcp.Tool{Name: "restack", Description: "Rebuild the landed stack on origin's base after the base moved, a bottom PR merged, or the stack sentinel flagged the stack (stack_at_risk in status): rebases in train order, moves the branches, pushes and retargets PRs. A conflict moves nothing and goes back to the task that owns the commit. A broken stack is fixed with restack, never with git or a worker."},
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
				Base:    res.Base, Moves: res.Moves, Skipped: res.Merged, Superseded: res.Superseded, Retargeted: res.Retargeted, Dropped: res.Dropped,
			}, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "unstack", Description: "Take a landed task out of the PR stack for good, by task id, PR URL or PR number: for work that should not ship as its own PR (superseded, re-landed elsewhere, abandoned). Its commits leave integration on the next restack and Saddle never touches its PR again. Merged and closed PRs and killed tasks leave the stack by themselves; this is the escape hatch for the rest. Never edit state.db instead."},
		func(_ context.Context, _ *mcp.CallToolRequest, in UnstackIn) (*mcp.CallToolResult, OK, error) {
			t, err := a.Unstack(in.Task)
			if err != nil {
				return nil, OK{}, err
			}
			_, _ = sentinel.New(a).Check() // update the flag now rather than in two minutes
			return nil, OK{Message: fmt.Sprintf("%s is out of the PR stack; run restack to drop its commits from integration.", t.ID)}, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "sentinel_ack", Description: "Acknowledge the stack sentinel's current at-risk flag when restack can't fix it: prs and land stop holding work back and the needs-human labels come off, until a different layer breaks or the stack checks clean. Never edit state.db instead."},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, OK, error) {
			f, err := a.AckFlag()
			if err != nil {
				return nil, OK{}, err
			}
			_, _ = sentinel.New(a).Check() // take the labels off now
			return nil, OK{Message: fmt.Sprintf("Acknowledged the flag from %s up (%s).", f.Task, f.Cause)}, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "requeue", Description: "Put a landed task whose work is missing from integration back in the merge train, recreating its branch and worktree if needed; run land afterwards. Never edit state.db instead."},
		func(_ context.Context, _ *mcp.CallToolRequest, in TaskIn) (*mcp.CallToolResult, OK, error) {
			if err := a.Requeue(in.Task); err != nil {
				return nil, OK{}, err
			}
			return nil, OK{Message: in.Task + " is queued again; run land."}, nil
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
