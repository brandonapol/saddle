// Package mcpserver is the stdio MCP server each agent gets. The calling task
// comes from SADDLE_TASK, so spawn sets the parent and claim/done apply to it.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/automerge"
	"github.com/brandonapol/saddle/internal/autopilot"
	"github.com/brandonapol/saddle/internal/sentinel"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type SpawnIn struct {
	Title  string   `json:"title" jsonschema:"short task title, e.g. 'meter ingestion worker'"`
	Prompt string   `json:"prompt" jsonschema:"self-contained instructions: goal, files, constraints, how to verify, done-when"`
	Claims []string `json:"claims,omitempty" jsonschema:"repo-relative path globs this task owns, e.g. internal/meter/**; must not overlap other live tasks"`
	Model  string   `json:"model,omitempty" jsonschema:"model for the adapter, e.g. claude's opus, sonnet or haiku; defaults to config for claude and to the CLI's default otherwise. Models in status' excluded_models (codex's astra by default) are refused"`
	// Adapter picks the agent CLI; see agent.ByName.
	Adapter string `json:"adapter,omitempty" jsonschema:"agent to run: claude, codex, gemini or grok, any one listed as available in the brief or status. Omitted means the session default (saddle up <agent>, else harness in config). Claude and grok run saddle hooks; codex and gemini claims are advisory. An unavailable adapter fails with the reason, never a substitute"`
	// Kind steers the adapter pick; see app.SpawnReq.Kind.
	Kind  string `json:"kind,omitempty" jsonschema:"task kind, e.g. docs, mechanical, design, review; without adapter it picks an adapter whose [adapters.<name>] kinds list it, else [adapters] policy picks (status shows adapter_policy and each adapter's pick rank)"`
	ID    string `json:"id,omitempty" jsonschema:"optional task id; defaults to the next tN"`
	Issue int    `json:"issue,omitempty" jsonschema:"GitHub issue number this task implements; its PR will close it"`
	// After becomes explicit stack edges; see app.SpawnReq.After.
	After []string `json:"after,omitempty" jsonschema:"ids of unmerged tasks this one builds on, e.g. [\"t61\"] when it uses a make target or API t61 adds; its PR stacks on theirs even when their files don't overlap"`
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
	Task string  `json:"task" jsonschema:"task id"`
	Text string  `json:"text,omitempty" jsonschema:"text to type followed by Enter, e.g. an answer to the agent's question"`
	Keys KeyList `json:"keys,omitempty"` // schema in keysSchema: a string or a list
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

type ReleaseIn struct {
	Paths []string `json:"paths,omitempty" jsonschema:"claimed globs to give up, exactly as claimed; empty releases all of them"`
	Task  string   `json:"task,omitempty" jsonschema:"orchestrator only: the task whose claims to release; defaults to your own"`
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
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Status   string    `json:"status"`
	Reason   string    `json:"reason,omitempty"` // why a spawn failed
	Model    string    `json:"model,omitempty"`
	Adapter  string    `json:"adapter,omitempty"` // claude, codex, gemini or grok (#480)
	Parent   string    `json:"parent,omitempty"`
	Branch   string    `json:"branch,omitempty"`
	Claims   []string  `json:"claims,omitempty"`
	Train    string    `json:"train,omitempty"`
	Notices  int       `json:"pending_notices,omitempty"`
	PR       string    `json:"pr,omitempty"`
	Window   string    `json:"window,omitempty"`
	Worktree string    `json:"worktree,omitempty"`
	created  time.Time // for the status tool's since filter
}

type StatusOut struct {
	// Headline is the one-sentence state of the fleet and its colour (#403).
	Headline    app.StateHeadline `json:"headline"`
	Integration string            `json:"integration"`
	Warnings    []string          `json:"warnings,omitempty"`
	StackAtRisk *StackRisk        `json:"stack_at_risk,omitempty"`
	// CIRed lists the PRs whose CI is red and the layers each holds back.
	CIRed []app.CIRedHold `json:"ci_red,omitempty"`
	// DoneOrphaned lists the workers that called done, were never queued and
	// went quiet, with the recovery step saddle is on (#467).
	DoneOrphaned []app.DoneOrphan `json:"done_orphaned,omitempty" jsonschema:"workers that called done but were never queued and went quiet; step: regated, nudged (finished ones leave the list)"`
	// OrchestratorContext is how full the orchestrator's context is; absent
	// before its transcript has a reading.
	OrchestratorContext *OrchContext `json:"orchestrator_context,omitempty"`
	// HeavyRuns is the machine's heavy-run queue (saddle run); absent while
	// it is idle.
	HeavyRuns *app.HeavyRuns `json:"heavy_runs,omitempty" jsonschema:"the machine's heavy-run queue per class: slots, holders (task, repo, cmd, age, overdue past max_run) and waiters (position, task, wait, eta); absent while idle"`
	// Adapters says which coding CLIs a worker can run on here (#182).
	Adapters []agent.Status `json:"adapters" jsonschema:"each adapter (claude, codex, gemini, grok) and whether it can run here: CLI on PATH and logged in; reason when not. pick_rank orders them for the next spawn without adapter= (1 first, absent when it can't be picked) and pick says why. excluded_models are models the owner excluded: spawn refuses them, so never pass one as model="`
	// AdapterPolicy is [adapters] policy and failover in words (#481).
	AdapterPolicy string `json:"adapter_policy,omitempty" jsonschema:"how spawns without adapter= pick (order, cost or quota) and what a worker does when its adapter runs out mid-task (handoff to the next adapter, or park until the reset)"`
	// Fallback is the [fallback] chains (#483).
	Fallback []app.FallbackStatus `json:"fallback,omitempty" jsonschema:"[fallback] chains: from (adapter or adapter:model), the chain that takes over while it is out of quota, out_of_quota_until while it is, now (the entry new spawns and handed-off workers go to) and skipped (entries passed over: out, excluded or unable to run here)"`
	Tasks    []TaskView           `json:"tasks"`
	// Hidden counts tasks the compact status tool left out, by status (#340).
	Hidden map[string]int `json:"hidden,omitempty" jsonschema:"tasks left out of the compact view, by status; ask with detail, state or ids to see them"`
	Hint   string         `json:"hint,omitempty"`
}

// CatchUpIn picks where a catch-up starts.
type CatchUpIn struct {
	Since string `json:"since,omitempty" jsonschema:"a duration back from now (2h, 30m) or an RFC 3339 time; empty: the owner's last look"`
}

// CatchUpOut is the catch-up.
type CatchUpOut struct {
	Since   time.Time `json:"since"`
	Bullets []string  `json:"bullets"`
}

// CatchUpSince parses since: a duration back from now, an RFC 3339 time, or
// "" for the owner's last look.
func CatchUpSince(a *app.App, since string, now time.Time) (time.Time, error) {
	since = strings.TrimSpace(since)
	if since == "" {
		return a.CatchUpSince(now), nil
	}
	if d, err := time.ParseDuration(since); err == nil {
		return now.Add(-d), nil
	}
	t, err := time.Parse(time.RFC3339, since)
	if err != nil {
		return time.Time{}, fmt.Errorf("since %q: want a duration like 2h or an RFC 3339 time", since)
	}
	return t, nil
}

// OrchContext is the orchestrator's context use against its compact threshold.
type OrchContext struct {
	Percent   int    `json:"percent"`    // of the context window in use
	Tokens    int64  `json:"tokens"`     // prompt size of the latest response
	Window    int64  `json:"window"`     // context window in tokens
	CompactAt int    `json:"compact_at"` // percent at which saddle compacts it
	Model     string `json:"model,omitempty"`
	Adapter   string `json:"adapter,omitempty"` // what the orchestrator runs on: claude or grok (#479)
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

// PublishIn is one publish; see app.PublishReq.
type PublishIn struct {
	Target     string `json:"target" jsonschema:"the task id (or branch) whose own commits to publish"`
	Base       string `json:"base,omitempty" jsonschema:"the branch the PR targets; defaults to the configured base"`
	BranchName string `json:"branch_name,omitempty" jsonschema:"the branch to push; defaults to saddle/<slug of the title>"`
	Draft      bool   `json:"draft,omitempty" jsonschema:"open the PR as a draft"`
}

// AutomergeIn steers the auto-merge watcher.
type AutomergeIn struct {
	Action string `json:"action" jsonschema:"on, off, status, hold or release"`
	Stack  string `json:"stack,omitempty" jsonschema:"for hold and release: the stack's name (its bottom task), a task id, PR URL or PR number"`
}

// AutopilotIn steers the autopilot driver.
type AutopilotIn struct {
	Action     string `json:"action" jsonschema:"on, off, status, pause, resume or infinite"`
	Mode       string `json:"mode,omitempty" jsonschema:"for infinite: on, off or status; defaults to status"`
	Until      string `json:"until,omitempty" jsonschema:"for on: stop spawning at this time (HH:MM, or RFC 3339)"`
	UntilUsage string `json:"until_usage,omitempty" jsonschema:"for on: stop spawning at this share of a plan-limit window (e.g. 90%)"`
	MaxTasks   int    `json:"max_tasks,omitempty" jsonschema:"for on: stop after spawning this many tasks; 0 means no limit"`
	ReadyLabel string `json:"ready_label,omitempty" jsonschema:"for on: label of the issues autopilot may pick up; defaults to saddle:ready"`
}

type ConcurrencyIn struct {
	Limit int  `json:"limit,omitempty" jsonschema:"new cap on running worker agents, 1 to 16; omit to only read it"`
	Reset bool `json:"reset,omitempty" jsonschema:"drop the runtime override and go back to the configured concurrency"`
}

type QueueMoveIn struct {
	Task     string `json:"task" jsonschema:"task id waiting in the merge train"`
	Position int    `json:"position" jsonschema:"1 lands next; past the end means the back"`
}

type QueueHoldIn struct {
	Task   string `json:"task" jsonschema:"task id waiting in the merge train"`
	Reason string `json:"reason,omitempty" jsonschema:"why it is held"`
}

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
	Message string           `json:"message,omitempty"`
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
	h, err := a.Headline(time.Now())
	if err != nil {
		return out, err
	}
	out.Headline = h
	f, flagged, err := a.Flag()
	if err != nil {
		return out, err
	}
	if flagged {
		out.StackAtRisk = &StackRisk{Task: f.Task, Cause: f.Cause, PRs: f.PRs, Acked: f.Acked, Fix: StackFix}
	}
	if out.CIRed, err = a.CIRedHolds(); err != nil {
		return out, err
	}
	if out.DoneOrphaned, err = a.FlaggedOrphans(); err != nil {
		return out, err
	}
	if u, err := a.OrchestratorContext(); err == nil && u.Prompt > 0 {
		out.OrchestratorContext = &OrchContext{Percent: int(math.Round(u.Fraction() * 100)), Tokens: u.Prompt,
			Window: u.Window, CompactAt: int(math.Round(a.CompactAt() * 100)), Model: u.Model, Adapter: u.Adapter}
	}
	if v, err := a.HeavyRuns(); err != nil {
		out.Warnings = append(out.Warnings, "heavy-run queue: "+err.Error())
	} else if v.Busy() {
		out.HeavyRuns = &v
	}
	out.Adapters = a.Adapters()
	out.AdapterPolicy = a.AdapterPolicy()
	out.Fallback = a.Fallbacks(time.Now())
	out.Tasks, err = Tasks(a)
	return out, err
}

// Tasks lists every task with its claims, train state and pending notices.
// It reads the store, and tmux only for live tasks gone quiet for
// app.OrphanAfter, so the TUI can poll it every second.
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
	orphans, err := a.Orphans()
	if err != nil {
		return nil, err
	}
	out := make([]TaskView, 0, len(ts))
	for _, t := range ts {
		reason := ""
		if t.Status == app.StatusFailed {
			reason = t.Summary
		}
		if orphans[t.ID] {
			t.Status, reason = app.StatusOrphaned, app.OrphanHint(t.ID)
		}
		out = append(out, TaskView{Reason: reason,
			ID: t.ID, Title: t.Title, Status: t.Status, Model: t.Model, Adapter: a.AdapterOf(t), Parent: t.Parent, Branch: t.Branch,
			Claims: cl[t.ID], Train: tr[t.ID], Notices: pending[t.ID], PR: t.PR, Window: t.Window, Worktree: t.Worktree,
			created: t.CreatedAt,
		})
	}
	return out, nil
}

// Serve runs task's MCP server on stdio. When the client goes away it still
// waits for any done gate it started (#368), so the task is queued even if
// Claude Code dropped the call.
func Serve(ctx context.Context, a *app.App, task string) error {
	err := New(a, task).Run(ctx, &mcp.StdioTransport{})
	a.WaitDoneJobs()
	return err
}

// ServeIdle runs a server with no tools whose instructions say why: the
// plugin's server in a session saddle can't orchestrate from.
func ServeIdle(ctx context.Context, why string) error {
	s := mcp.NewServer(&mcp.Implementation{Name: "saddle", Version: "0.1.0"}, &mcp.ServerOptions{Instructions: why})
	return s.Run(ctx, &mcp.StdioTransport{})
}

const serverInstructions = "Saddle coordinates parallel coding agents. Use spawn for disjoint sub-work, claim before large edits, and done when your branch is committed and tested. " +
	"When prs is blocked (the stack is flagged, or GitHub refuses a base change) but a task's work stands on its own, publish is the escape hatch: it opens that task's PR against base. Never git push or gh pr create by hand."

// New builds the MCP server for task without starting it.
func New(a *app.App, task string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "saddle", Version: "0.1.0"}, &mcp.ServerOptions{
		Instructions: serverInstructions,
	})
	self := func() error {
		if task == "" {
			return errors.New("SADDLE_TASK is not set; this MCP server must be started by saddle")
		}
		return nil
	}

	mcp.AddTool(s, &mcp.Tool{Name: "spawn", Description: fmt.Sprintf("Start a new parallel agent on its own branch and worktree in a new tmux window. Give it disjoint claims. When it builds on another task's unmerged work (a make target, file or API that task adds), pass after with that task's id so its PR stacks on that task's PR instead of failing CI on base. Sub-tasks are capped in depth below the orchestrator (spawn.max_depth, default %d) and in working children per task (spawn.max_children, default %d). Spawn refuses while the heavy-run queue is backed up; the error names the class and when it should clear.", app.DefaultMaxDepth, app.DefaultMaxChildren)},
		func(_ context.Context, _ *mcp.CallToolRequest, in SpawnIn) (*mcp.CallToolResult, SpawnOut, error) {
			if err := self(); err != nil {
				return nil, SpawnOut{}, err
			}
			t, err := a.Spawn(app.SpawnReq{ID: in.ID, Title: in.Title, Prompt: in.Prompt, Claims: in.Claims, Model: in.Model, Adapter: in.Adapter, Kind: in.Kind, Parent: task, Issue: in.Issue, After: in.After, Confirm: in.Confirm})
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

	mcp.AddTool(s, &mcp.Tool{Name: "release", Description: "Give up claims you no longer need so other tasks can use those paths. The orchestrator may pass task to release another task's claims. Errors when nothing matched."},
		func(_ context.Context, _ *mcp.CallToolRequest, in ReleaseIn) (*mcp.CallToolResult, OK, error) {
			if err := self(); err != nil {
				return nil, OK{}, err
			}
			owner := task
			if in.Task != "" && in.Task != task {
				if task != app.OrchestratorID {
					return nil, OK{}, fmt.Errorf("only the orchestrator can release another task's claims; ask it to free %s's", in.Task)
				}
				owner = in.Task
			}
			n, err := a.Store.ReleaseN(owner, in.Paths...)
			if err != nil {
				return nil, OK{}, err
			}
			if n == 0 {
				hint := ""
				if owner == app.OrchestratorID {
					hint = "; pass task to release another task's claim"
				}
				if len(in.Paths) == 0 {
					return nil, OK{}, fmt.Errorf("released nothing: %s holds no claims%s", owner, hint)
				}
				return nil, OK{}, fmt.Errorf("released nothing: %s holds no claim on %s (globs must match as claimed)%s", owner, strings.Join(in.Paths, ", "), hint)
			}
			return nil, OK{Message: fmt.Sprintf("released %d claim(s) of %s", n, owner)}, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "status", Description: "List saddle tasks with status and merge-train state, plus warnings, heavy_runs (the machine's heavy-run queue: holders and waiters per class) while it is busy, stack_at_risk when the stack sentinel has flagged the PR stack, and orchestrator_context: the adapter and model you run on, and how full your context is against the compact_at threshold. Compact by default: essentials per task, killed and merged tasks counted in hidden. Filter with state, ids or since; detail=true is the full dump with claims, branches and worktrees, and can be large."},
		func(_ context.Context, _ *mcp.CallToolRequest, in StatusIn) (*mcp.CallToolResult, StatusOut, error) {
			out, err := Status(a)
			if err != nil {
				return nil, out, err
			}
			out, err = statusView(out, in, time.Now())
			return nil, out, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "catchup", Description: "Catch me up: what happened since the owner last looked (their last keystroke in the TUI) or since a time you give, in at most 5 plain bullets: what waits on the owner first, then what is stuck, what got done, what started, and the state now. Built from the event log by saddle, not a model."},
		func(_ context.Context, _ *mcp.CallToolRequest, in CatchUpIn) (*mcp.CallToolResult, CatchUpOut, error) {
			now := time.Now()
			since, err := CatchUpSince(a, in.Since, now)
			if err != nil {
				return nil, CatchUpOut{}, err
			}
			bs, err := a.CatchUp(since, now)
			return nil, CatchUpOut{Since: since, Bullets: bs}, err
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

	mcp.AddTool(s, &mcp.Tool{Name: "done", Description: "Finish your task: requires a clean, committed worktree. Queues your branch in the merge train. When the repo's gate has to check your tree, it returns at once with a job id and checks in the background; you get a [saddle] notice when the job ends. Calling done again while it runs starts no second check."},
		func(_ context.Context, _ *mcp.CallToolRequest, in DoneIn) (*mcp.CallToolResult, OK, error) {
			if err := self(); err != nil {
				return nil, OK{}, err
			}
			job, err := a.DoneAsync(task, in.Summary)
			if err != nil {
				return nil, OK{}, err
			}
			return nil, OK{Message: doneMessage(job)}, nil
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
			rs, err := a.LandRecorded()
			if err != nil {
				return nil, LandOut{}, err
			}
			return nil, LandOut{Message: landMessage(len(rs)), Results: rs}, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "prs", Description: "Push landed branches and open or update a stack of GitHub PRs, in landing order."},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, PRsOut, error) {
			urls, err := a.PRs()
			return nil, PRsOut{PRs: urls}, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "publish", Description: "Publish one task's own commits, and nothing else, as an independent PR against base, outside the stack: the escape hatch when prs is blocked (stack flagged, or GitHub refuses a base change) but this task's work stands on its own. Saddle pushes a fresh branch itself; never git push or gh pr create by hand. It refuses when the commits need unmerged work from tasks below it."},
		func(_ context.Context, _ *mcp.CallToolRequest, in PublishIn) (*mcp.CallToolResult, OK, error) {
			res, err := a.Publish(app.PublishReq{Target: in.Target, Base: in.Base, BranchName: in.BranchName, Draft: in.Draft})
			if err != nil {
				return nil, OK{}, err
			}
			if res.Existing {
				return nil, OK{Message: "PR already open: " + res.URL}, nil
			}
			return nil, OK{Message: fmt.Sprintf("Published %s as %s: %s", in.Target, res.Branch, res.URL)}, nil
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

	mcp.AddTool(s, &mcp.Tool{Name: "sentinel_ack", Description: "Acknowledge the stack sentinel's current at-risk flag and every ci-red hold when restack and repairs can't fix them: prs and land stop holding work back and the needs-human labels come off, until a different layer breaks, a layer goes red on a new head, or the stack checks clean. Auto-merge still never merges a red PR. Never edit state.db instead."},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, OK, error) {
			red, err := a.AckCIRed()
			if err != nil {
				return nil, OK{}, err
			}
			var msg []string
			for _, l := range red {
				msg = append(msg, fmt.Sprintf("Acknowledged ci-red on %s: prs and land stop holding the layers above it.", l.Task))
			}
			f, err := a.AckFlag()
			if err != nil {
				if len(red) > 0 {
					return nil, OK{Message: strings.Join(msg, "\n")}, nil // only red CI was holding work back
				}
				return nil, OK{}, err
			}
			_, _ = sentinel.New(a).Check() // take the labels off now
			msg = append(msg, fmt.Sprintf("Acknowledged the flag from %s up (%s).", f.Task, f.Cause))
			return nil, OK{Message: strings.Join(msg, "\n")}, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "requeue", Description: "Put a landed task whose work is missing from integration back in the merge train, recreating its branch and worktree if needed; run land afterwards. Never edit state.db instead."},
		func(_ context.Context, _ *mcp.CallToolRequest, in TaskIn) (*mcp.CallToolResult, OK, error) {
			if err := a.Requeue(in.Task); err != nil {
				return nil, OK{}, err
			}
			return nil, OK{Message: in.Task + " is queued again; run land."}, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "automerge", Description: "Auto-merge of ready PR stacks (off by default): on merges the bottom PR of a ready stack (green CI, mergeable and CLEAN, not a draft, not needs-human, not at risk), restacks and repeats; off leaves PRs to the owner; status shows the stacks as a graph and what each waits on; hold <stack> keeps one stack from merging while it stays tracked and restacked; release lets it merge. A failed merge stops it until on is called again. Only turn it on or release a hold when the owner asked."},
		func(_ context.Context, _ *mcp.CallToolRequest, in AutomergeIn) (*mcp.CallToolResult, automerge.Status, error) {
			w := a.NewAutomerge(nil)
			var err error
			switch in.Action {
			case "on", "off":
				err = w.SetEnabled(in.Action == "on")
			case "hold", "release":
				if strings.TrimSpace(in.Stack) == "" {
					return nil, automerge.Status{}, fmt.Errorf("%s needs a stack, task or PR", in.Action)
				}
				if in.Action == "hold" {
					_, err = a.AutomergeHold(in.Stack)
				} else {
					_, err = a.AutomergeRelease(in.Stack)
				}
			case "status", "":
				if st, err := w.Plan(); err == nil {
					return nil, st, nil
				}
			default:
				return nil, automerge.Status{}, fmt.Errorf("unknown action %q: want on, off, status, hold or release", in.Action)
			}
			if err != nil {
				return nil, automerge.Status{}, err
			}
			st, err := w.Status()
			return nil, st, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "autopilot", Description: "Autopilot (off by default): while on, saddle itself lands, tops up from open issues labelled ready up to the concurrency cap and nudges you when the pipeline stalls. on starts a run (optional until, until_usage, max_tasks, ready_label); off ends it; pause and resume hold and continue it; status reads it. infinite (mode on, off or status) is autopilot with no stop condition, as saddle autopilot infinite: an empty ready queue asks you to find more work, a full plan-limit window parks tasks until it resets; on over a running bounded run keeps that run, off ends the run. Only turn it on when the owner asked."},
		func(_ context.Context, _ *mcp.CallToolRequest, in AutopilotIn) (*mcp.CallToolResult, autopilot.State, error) {
			d := a.NewAutopilot(nil)
			var st autopilot.State
			var err error
			switch in.Action {
			case "on":
				var o autopilot.Options
				if in.Until != "" {
					if o.Stop.Until, err = autopilot.ParseUntil(in.Until, time.Now()); err != nil {
						return nil, st, err
					}
				}
				if in.UntilUsage != "" {
					if o.Stop.UntilUsage, err = autopilot.ParseUsage(in.UntilUsage); err != nil {
						return nil, st, err
					}
				}
				if in.MaxTasks < 0 {
					return nil, st, fmt.Errorf("max_tasks %d: want 0 (no limit) or more", in.MaxTasks)
				}
				o.Stop.MaxTasks, o.ReadyLabel = in.MaxTasks, in.ReadyLabel
				st, err = d.Enable(o)
			case "off":
				st, err = d.Disable()
			case "pause":
				st, err = d.Pause()
			case "resume":
				st, err = d.Resume()
			case "infinite":
				switch in.Mode {
				case "on", "off":
					st, err = d.SetInfinite(in.Mode == "on")
				case "status", "":
					st, err = d.Status()
				default:
					err = fmt.Errorf("infinite mode %q: want on, off or status", in.Mode)
				}
			case "status", "":
				st, err = d.Status()
			default:
				err = fmt.Errorf("unknown action %q: want on, off, status, pause, resume or infinite", in.Action)
			}
			return nil, st, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "concurrency", Description: fmt.Sprintf("Read or change how many worker agents may run at once (the bots limit): running count, limit, and whether it comes from config or a runtime override. limit (%d-%d) overrides config until reset; spawn honors it at once. Lowering it stops nothing that runs; new spawns wait until fewer run. Only change it when the owner asks.", app.MinConcurrency, app.MaxConcurrency)},
		func(_ context.Context, _ *mcp.CallToolRequest, in ConcurrencyIn) (*mcp.CallToolResult, app.Concurrency, error) {
			switch {
			case in.Reset:
				c, err := a.ResetConcurrency()
				return nil, c, err
			case in.Limit != 0:
				c, err := a.SetConcurrency(in.Limit)
				return nil, c, err
			}
			c, err := a.Concurrency()
			return nil, c, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "queue_move", Description: "Move a branch waiting in the merge train to a position in the queue (1 lands next), e.g. to land a fix before the work that needs it."},
		func(_ context.Context, _ *mcp.CallToolRequest, in QueueMoveIn) (*mcp.CallToolResult, OK, error) {
			if err := a.MoveInQueue(in.Task, in.Position); err != nil {
				return nil, OK{}, err
			}
			return nil, OK{Message: fmt.Sprintf("moved %s", in.Task)}, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "queue_hold", Description: "Keep a branch waiting in the merge train from landing, without losing its place, until queue_release. Calling done again doesn't release it."},
		func(_ context.Context, _ *mcp.CallToolRequest, in QueueHoldIn) (*mcp.CallToolResult, OK, error) {
			if err := a.Hold(in.Task, in.Reason); err != nil {
				return nil, OK{}, err
			}
			return nil, OK{Message: in.Task + " is on hold; queue_release lets it land."}, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "queue_release", Description: "Let a branch held with queue_hold land again, from its place in the queue; run land afterwards."},
		func(_ context.Context, _ *mcp.CallToolRequest, in TaskIn) (*mcp.CallToolResult, OK, error) {
			if err := a.Unhold(in.Task); err != nil {
				return nil, OK{}, err
			}
			return nil, OK{Message: in.Task + " is back in line; run land."}, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "peek", Description: "Read the last lines of a task's Claude Code terminal, e.g. to see what it is stuck on or what a prompt is asking."},
		func(_ context.Context, _ *mcp.CallToolRequest, in PeekIn) (*mcp.CallToolResult, PeekOut, error) {
			out, err := a.Peek(in.Task, in.Lines)
			return nil, PeekOut{Screen: out}, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "send_keys", InputSchema: keysSchema, Description: "Type into a task's terminal: answer its question (text) or pick a prompt option (keys). Claude Code menus: a numbered menu takes the digit alone (keys \"1\" or [\"1\"]); an unnumbered menu needs arrows then Enter (keys [\"Down\",\"Enter\"]). Only answer when it is clearly safe or the user told you what to answer. Peek first, and peek again afterwards to confirm the prompt is gone."},
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

// doneMessage is what the done tool tells the agent about job (#368).
func doneMessage(job app.DoneJob) string {
	switch {
	case job.NoChange:
		return "Your branch has no commits the integration branch lacks, so there is nothing to land. The task is finished with no change and the orchestrator was told. You can stop now."
	case job.Joined:
		return fmt.Sprintf("Done gate %s is already checking your branch; no second check started. You can stop now: a [saddle] notice says when it is queued, or why not.", job.ID)
	case job.Running:
		return fmt.Sprintf("Done gate %s is checking your branch in the background; it is queued in the merge train when the gate passes. You can stop now: a [saddle] notice says when it is queued, or why not. Don't call done again unless that notice asks.", job.ID)
	}
	return "Queued in the merge train. You can stop now. If it conflicts or fails tests, you will be told."
}
