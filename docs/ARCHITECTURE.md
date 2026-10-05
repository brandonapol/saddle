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

Saddle runs in-process. There is no `saddled` daemon in the MVP (decision #5):
every `saddle` command, hook and MCP server opens the repo's SQLite database
(`.saddle/state.db`, WAL mode with a busy timeout) and works on it directly.
tmux keeps the agents alive between commands.

```
tmux session saddle-<repo>             (agents; survive saddle up exiting)
├── 1:T3-meter  claude ── saddle mcp ─┐
│               └ hooks → saddle hook ┤
├── 2:T4-invoice claude …             ├── .saddle/state.db (sqlite)
├── 3:T5-stripe codex …               │   tasks, claims, events, train,
└── …                                 │   usage, notices
                                      │
saddle up  (TUI + orchestrator chat) ─┤   while it runs: stack sentinel,
saddle land | prs | sync | …         ─┘   CI watcher
```

- **`saddle up`** is the TUI. It polls the store, and while it runs it also
  hosts the long-lived loops: the stack sentinel and the CI watcher.
- **`saddle hook <event>`** is the Claude Code hook entrypoint. It opens the
  store, returns a verdict, and must be fast. It fails open if it can't.
- **`saddle mcp`** is a stdio MCP server, one per agent. Each knows its task
  id from env. Through it, agents can `spawn`, `claim`, `release`, `status`,
  `brief`, `ask_owner` and call `done`.
- **`saddle doctor`** checks that a repo is ready for all of the above.
- **The Claude Code plugin** (`plugin/`, listed by `.claude-plugin/marketplace.json`)
  makes the user's own Claude Code session the orchestrator instead of the
  TUI's. Its MCP server is `saddle plugin mcp` acting as task `t0`.
  `saddle plugin engine` (`internal/engine`) runs the TUI's background work
  headless: the watchers above, plus noticing prompts and stalls, which it
  queues as `t0` notices. `saddle plugin wait`, run in the background,
  exits when one needs the session, and `saddle plugin hook` delivers them
  on the session's prompts and tool calls. The engine and `saddle up` share
  `.saddle/tui.lock`, which records its holder, so only one of them drives
  `t0`. The plugin's hook and MCP server do nothing outside a repo that ran
  `saddle init` or inside saddle's own agents (`SADDLE_TASK` set).

Concurrent processes coordinate through SQLite transactions, not a socket.
The reactor, git watcher and dispatcher described below are the target design;
today their decisions are made inline in `internal/app`.

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
- **Publishing outside the stack (#220).** When `prs` is blocked but one
  task's work is fine on its own, `saddle publish <task|branch> [--base main]
  [--branch-name name] [--draft]` replays only that task's commits (checked by
  patch-id) onto base, pushes them to a fresh branch and opens an independent
  PR with the repo's PR template and `Closes #N`. The saddle process pushes as
  the train, so the ref guard allows it and the model never runs `git push`.
  It refuses work that needs unmerged tasks below it, and prints the URL when
  the PR exists. `prs` leaves that task's PR alone; the layers above it wait
  until it merges.

## Agents

`Adapter{Launch, Inject, Usage}`.

- **Claude Code**: Saddle writes a per-worktree `.claude/settings.local.json`
  that wires hooks to `saddle hook` and registers `saddle mcp`. It then
  launches `claude --model <m>` with the task brief.
  The orchestrator's settings carry an allowlist (`agent.OrchestratorAllow`)
  so neither a prompt nor the auto-mode classifier blocks what it is meant to
  run: `saddle` (by name and absolute path), `gh pr` and `gh issue`, `git
  fetch` and read-only git. `git push`, `merge`, `rebase`, `reset` and
  `checkout` are removed (`agent.OrchestratorDeny`); `saddle publish` is the
  way to push. `saddle doctor` checks both.
- **Grok CLI** (`harness = "grok"`): same task, worktree and tmux window.
  Saddle writes `.grok/hooks/saddle.json` and a `[mcp_servers.saddle]` block
  (both gitexcluded) and launches `grok --trust` with the brief as `--rules`.
  The orchestrator has no long-lived stdin protocol, so `saddle grok-bridge`
  runs one `grok -p` turn per chat message and resumes the session. Output is
  `streaming-messages-json`, which matches the Claude stream the TUI parses.
  `[adapters.grok]` cmd and args apply here too.
- **Codex / one-shot Grok** (`adapter=codex|grok` under the Claude harness):
  hookless, so claims are advisory and notices are typed in. Usage and status
  come from their output on a best-effort basis.

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
  worktrees/<task>/  one per agent, branch saddle/<task>
```

## Code layout

```
cmd/saddle/          single binary
internal/app/        core: spawn, claims, merge train, stacks, notices
internal/cli/        cobra commands
internal/config/     TOML loading and merging
internal/store/      sqlite schema and migrations
internal/doctor/     saddle doctor preflight checks
internal/gitx/       worktrees, watcher, rename detection, train, stacks
internal/claims/     glob claims and overlap
internal/planner/    epic → DAG, static checker
internal/agent/      adapters: claude, codex, grok
internal/tmux/       session and window control, capture-pane
internal/mcp/        stdio MCP server
internal/hook/       hook entrypoint
internal/narrator/   haiku narrator, usage
internal/tui/        bubble tea views
internal/engine/     headless watcher for the plugin orchestrator
plugin/              Claude Code plugin (MCP, hooks, /saddle:* commands)
```

## Dogfooding order (M0)

1. Scaffold, config, store (#2–#4). No daemon: Saddle runs in-process (#5).
2. Worktrees and tmux, then the Claude adapter with hooks and MCP (#13 #14 #15
   #21 #22). At this point Saddle can open agents and you can watch them.
3. Claims and PreToolUse enforcement (#20). This removes the
   overwriting-each-other failure.
4. Serial train, rename map and stacked PRs (#25 #26 #52). This removes the
   manual rebasing and the moved-directory failure.
5. Planner (#7–#9) and the control TUI (#33 #34). From here you hand Saddle an
   epic and let it run.
6. Reactor and git watcher (#45 #46), then the forecaster and dispatcher.
