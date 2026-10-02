# Saddle architecture

Saddle runs a group of coding agents (mostly Claude Code, plus Codex and Grok
when a task calls for them) from one terminal. You give it an epic. It plans a
task graph, opens one tmux window and one git worktree per agent, keeps the
agents from colliding, and lands their work serially as stacked PRs.

The design rule is: **never use the expensive model as the synchronizer.**
Isolation, claims, a git-aware scheduler and a serial merge train handle sync.
The models write code.

TUI design: https://claude.ai/artifact/TKRvnLJoDLBonpdKDU19L4
Issues: epics #1 #6 #12 #19 #24 #32 #38 #44; milestone `M0: dogfood`.

## Processes

```
tmux session saddle-<repo>
├── 0:control   saddle tui ───────────┐
├── 1:T3-meter  claude ── saddle mcp ─┤
│               └ hooks → saddle hook ┤   JSON-RPC over
├── 2:T4-invoice claude …             ├── .saddle/saddled.sock
├── 3:T5-stripe codex …               │
└── …                                 │
                                      ▼
                               saddled (daemon)
             ┌──────────┬───────────┼───────────┬───────────┐
          reactor   git watcher  merge train  narrator    state.db
         (event loop) (fsnotify)  (serial)    (haiku)     (sqlite)
```

- **`saddled`** owns all state and all decisions. Everything else is a client.
- **`saddle tui`** is window 0. It subscribes to daemon events and renders.
- **`saddle hook <event>`** is the Claude Code hook entrypoint. It forwards the
  hook JSON to the daemon and returns its verdict. It must be fast, and it
  fails open if the daemon is down.
- **`saddle mcp`** is a stdio MCP server, one per agent. Each knows its task
  id from env. Through it, agents can `spawn`, `claim`, `release`, `status`,
  `brief`, `ask_owner` and call `done`.

## The reactor (event loop)

One goroutine makes every scheduling decision. That makes decisions totally
ordered, logged and replayable.

Inputs: hook events, git watcher deltas, MCP calls, train transitions, timers.
Outputs: `launch`, `hold`, `release`, `notify`, `reorder`, `escalate`.

Every output is written to `events` with a `reason`. The narrator and the TUI
read that log to explain what is happening.

### Collision handling, from cheapest to most expensive

1. **Plan time (static).** Claims are path globs per task. Overlapping claims
   become dependency edges. Moves and renames become barrier tasks, which run
   alone in wave 0. Writes to `serial` globs (`go.sum`, migrations) belong to
   the train.
2. **Edit time (enforced).** PreToolUse denies writes outside a task's claims
   or inside a held path, with a reason that names the owner. The agent adapts
   instead of colliding.
3. **Runtime forecast (deterministic).** The git watcher tracks every
   worktree: dirty files, touched symbols, renames in progress, ahead/behind.
   The forecaster scores task pairs (same symbol > same file > import
   neighbour). Clear cases are decided by rule.
4. **Dispatcher (Sonnet-class).** It is consulted only for gray-zone forecasts
   or would-be deadlocks. It returns structured output:
   `hold | proceed | sequence | ask_owner | escalate` with a wait-until
   condition (`commit(task, paths)`, `landed(task)`, `timeout`). Holds release
   automatically when the git watcher sees the condition.

To give gates something to wait on, agents are nudged to make small commits.
Saddle also snapshots dirty trees to `refs/saddle/wip/<task>` without
touching the agent's index.

## Merge train

There is an integration branch `saddle/epic-<n>`, and branches land on it one
at a time: rebase → test → fast-forward. When a conflict appears, Saddle
works through these steps and stops at the first one that resolves it:

1. Rebase with the **rename map**. Renames from every landed commit are
   recorded, applied to every live worktree, rewritten into claim globs, and
   announced to the affected agents.
2. Structural (tree-sitter) merge driver.
3. Regenerate derived files (lockfiles, codegen) instead of merging them.
4. Return the conflict to the **producing agent**, along with the other
   side's diff.
5. Escalate to the human (▲ needs-you) after two failed attempts.

None of these steps uses the planner model by default. The TUI shows a counter
of Opus tokens spent on merges, and the goal is to keep it at zero.

**Output is stacked PRs by default.** Tasks that depend on each other, or that
the planner clusters as the same topic, form a stack (DAG order, barriers at
the bottom). Unrelated work gets separate stacks. Saddle restacks a stack when
a lower layer changes and retargets it when a lower layer merges.

### Stack upkeep when people use GitHub (#119, #123)

The stack is only the landed tasks whose train entry is `landed` and that
are not killed. Saddle expects people to merge in the GitHub UI, squash,
close PRs and delete branches, and it handles those without anyone editing
`state.db`:

- **Leaving the stack.** Before `prs`, `restack` or a sentinel check reads
  the stack, `ReconcileStack` asks GitHub about each PR. A PR merged into base
  (merge, squash or rebase) moves its train entry to `merged`. A closed PR or
  a killed task moves it to `superseded`. Neither comes back, gets labeled, or
  is retargeted. Restack doesn't replay a superseded task's commits, so they
  leave integration and are never bundled into the next task's PR.
- **Merged into a stacked base.** A PR merged into the branch below it instead
  of base is merged if base has its work anyway (by patch-id or file content).
  Otherwise its task loses that PR, the next `prs` opens a fresh one, and the
  orchestrator is told once.
- **Self-healing refs.** A stacked task's deleted local branch is recreated at
  its landed commit. A push to a branch GitHub deleted drops the stale lease.
  A train note with no usable range (older binaries, #123) is rebuilt from the
  task's `landed` event. An empty range never counts as "merged": only restack
  proving base has the work does that.
- **Freezing only what is affected.** The sentinel flag makes `prs` publish
  the layers below the first broken one and stop there. `land` holds back only
  queued work that changes files the broken layers changed.
- **needs-human means a decision.** Only a conflict labels PRs, and the comment
  names the conflict. Base moving, a merged bottom PR or a drifted branch just
  flag the stack for `restack`.
- **Escape hatches.** `saddle unstack <task|pr>` takes a task out of the stack.
  `saddle sentinel ack` acknowledges the current flag, which lifts its freeze
  and labels until a different layer breaks. `saddle requeue <task>` puts back
  a landed task whose work integration lacks. Each has an MCP tool:
  `unstack`, `sentinel_ack`, `requeue`.

## Agents

`Adapter{Launch, Inject, Usage}`.

- **Claude Code**: Saddle writes a per-worktree `.claude/settings.local.json`
  that wires hooks to `saddle hook` and registers `saddle mcp`. It then
  launches `claude --model <m>` with the task brief.
- **Codex / Grok**: same interface. Usage and status come from their output on
  a best-effort basis. Grok is used for image-generation tasks.

Sub-agents spawned through MCP are ordinary tasks with a `parent_task`. Each
gets its own worktree, window and claims, and the train lands it before its
parent. Spawn depth and fan-out are capped.

## Narrator and usage

The narrator (`claude-haiku-4-5`) reads compact event deltas, never raw pane
text unless you ask a question about a window. It writes one line per salient
change into a single thread. Usage comes from Claude Code transcript JSONL
`usage` fields, bucketed per minute by task and model. Plan-limit bars are
estimates against caps you configure.

## Layout on disk

```
<repo>/.saddle/
  config.toml        per-repo config (merged over ~/.config/saddle/config.toml)
  state.db           sqlite: epics, tasks, deps, claims, events, usage, train, renames
  saddled.sock
  worktrees/<task>/  one per agent, branch saddle/<task>
```

## Code layout

```
cmd/saddle/          single binary; `saddle daemon` runs saddled
internal/cli/        cobra commands
internal/config/     TOML loading and merging
internal/store/      sqlite schema and migrations
internal/daemon/     socket RPC, pub/sub
internal/reactor/    event loop, holds, decisions
internal/gitx/       worktrees, watcher, rename detection, train, stacks
internal/claims/     glob claims and overlap
internal/planner/    epic → DAG, static checker
internal/agent/      adapters: claude, codex, grok
internal/tmux/       session and window control, capture-pane
internal/mcp/        stdio MCP server
internal/hook/       hook entrypoint
internal/narrator/   haiku narrator, usage
internal/tui/        bubble tea views
```

## Dogfooding order (M0)

1. Scaffold, config, store, daemon (#2–#5).
2. Worktrees and tmux, then the Claude adapter with hooks and MCP (#13 #14 #15
   #21 #22). At this point Saddle can open agents and you can watch them.
3. Claims and PreToolUse enforcement (#20). This removes the
   overwriting-each-other failure.
4. Serial train, rename map and stacked PRs (#25 #26 #52). This removes the
   manual rebasing and the moved-directory failure.
5. Planner (#7–#9) and the control TUI (#33 #34). From here you hand Saddle an
   epic and let it run.
6. Reactor and git watcher (#45 #46), then the forecaster and dispatcher.
