# Saddle v2 architecture

Status: proposed (ADR 0013). Tracking epic: #955.

Saddle v2 is a harness of harnesses. One agent CLI (Claude Code, Codex or
Grok) is the orchestrator. It hands tasks to worker agents on any of the
three, each in its own git worktree, and saddle gets their work onto main as
one PR per task without anyone rebasing, retargeting or merging by hand.

v2 is a rewrite, not a refactor. v1 is about 120,000 lines of Go outside
tests, around 40 state files beside the database, five lock files and two
loop hosts. v2's budget is 20,000 lines, one database, one lock and one loop.

## What v2 keeps from v1

These are the things that make saddle worth using. Everything else is
negotiable.

1. **One parent agent.** You talk to one chat; it plans, spawns and reports.
2. **Any of three CLIs in either role.** Orchestrator and workers run on
   claude, codex or grok, picked per task by difficulty and remaining quota.
3. **Isolation.** One task, one worktree, one branch, one PR (ADR 0012).
4. **Hands-off landing.** Nobody rebases by hand. A conflict or red CI goes
   back to the task that owns the branch. No model is ever the merger
   (ADR 0006).
5. **Approved work only.** Tickets enter through an explicit approval, and
   autopilot, infinite mode and loops keep the slots full from them.
6. **SQLite.** One file, any process can crash, restart reads the database.
7. **No latches.** Every blocked state has a self-service exit (#119).

## What v2 drops

| Dropped | Why | What covers the need |
|---|---|---|
| tmux workers and screen reading | Every state heuristic and timeout in ADR 0004's cluster C6 came from guessing at a screen | Headless turns with a typed result (below) |
| Hooks, path claims, symbol claims | Enforcement needs hooks, which only some CLIs have, and 546 unactioned `stack_warn` events show nobody acted on them | Schedule-time overlap on `Touches`, and fix in place |
| Stacked PRs, `after` on a branch | Retargets and `rebase --onto` are where the PR graph drifts from git | `after` waits for the merge; every PR targets main |
| Saddle's own merge queue and rebases | It re-implements the forge and bypasses repo rules | The forge merges: native auto-merge or merge queue |
| State files (`automerge.json`, `decisions.json`, …) | Shared tmp names, lost updates (#921), state nobody can query | Tables in `state.db` |
| Event-replayed status | Implicit state (#827) | One stored state per task, one transition function |
| `saddle up` hosting the loops | The TUI does I/O, so it is slow, and loops stop when it closes | One engine process; the TUI is a pure reader |
| Narrator, forecast, sentinel, planner DAG, runq, SRE mode, remote control, SSO, bridges, decision cards, checkpoints | Each is real work but none is the core loop | Out of 2.0. Each can return as a ticket against this design |

## Rules

A reviewer can check code against these.

1. **One store.** All state is rows in `.saddle/state.db`. The only other
   things saddle writes are worktrees, git refs, per-attempt log files
   (append-only) and one lock file. A test fails the build when a package
   other than `store` writes under `.saddle/`.
2. **One state per task, stored.** A task's state is a column, changed only
   by `core.Transition`, in the same transaction as its event row. Nothing
   derives state by replaying events. Events are an audit log.
3. **Observe, plan, apply.** The engine reads the world (database, forge,
   processes), calls a pure `core.Plan(observation) []Action`, and applies
   each action idempotently. Facts the forge owns (PR open, merged, CI,
   mergeable) are read from the forge every time and cached only for display.
4. **No latch.** A failed action is a row in `retries` keyed by the action
   and a hash of its inputs. It backs off and resets when an input changes.
   A table test lists every non-terminal state and reason with its exits,
   one of which is always a command the owner or orchestrator can run.
5. **The forge merges.** Saddle opens a PR and asks the forge to merge it
   when the repo's rules are met. Saddle never merges, never posts a status
   and never rebases another task's branch locally.
6. **A worker is turns, not a terminal.** An agent runs as non-interactive
   invocations. A turn ends when the process exits. Saddle never reads a
   screen and never answers a prompt.
7. **The UI does no I/O in its update loop.** It never calls git, the forge
   or an agent CLI at all.
8. **Agents and the orchestrator are the same thing.** One driver contract
   and one runner serve both roles. The roles differ in tools and working
   directory.

## Processes

```
 you ── saddle up (TUI) ──────────────┐ reads state.db and attempt logs,
                                      │ writes inbox and settings rows
 your own claude/codex/grok session   │
   └─ saddle mcp --role orchestrator ─┤
                                      ▼
                          .saddle/state.db (SQLite, WAL)
                                      ▲
 saddle engine  (one per repo) ───────┤ observe → plan → apply
   ├─ forge (gh): one batched read per tick
   ├─ git: worktrees, fetch, push
   └─ starts ─▶ saddle turn <attempt>   (one per running turn, detached)
                   ├─ claude -p | codex exec | grok -p   in the worktree
                   │     └─ saddle mcp --role worker ────┘ finish, ask
                   └─ .saddle/logs/<task>/<attempt>.jsonl
```

- **`saddle engine`** is the only process that runs timers, talks to the
  forge, starts turns and pushes. It holds `.saddle/engine.lock` (a flock,
  so a dead engine never wedges the next). Any command that needs it starts
  it detached. It wakes on `PRAGMA data_version` changing, on a turn ending
  and on its forge poll interval.
- **`saddle turn <attempt>`** wraps one CLI invocation. It is its own
  session leader, so it outlives an engine restart. It writes the CLI's
  stream to the attempt's log, normalises it, and records the outcome in the
  database itself. It holds a flock on its log for as long as it runs: the
  engine learns that a turn died by taking that lock.
- **`saddle mcp`** is a stdio MCP server started by an agent CLI. It reads
  its role and task from env and works on the database directly.
- **`saddle up`** is the TUI: a reader with a few row writes.
- **`saddle <command>`** (spawn, message, answer, kill, retry, hold, mode,
  loop, status, logs, attach, doctor) writes rows and exits.

### Why this is not the daemon ADR 0005 rejected

ADR 0005 rejected a daemon because every command would depend on reaching it
over a socket, so its crash would take every command with it. Here no
process ever talks to the engine. Commands, the MCP server and the TUI read
and write SQLite; the engine notices. With the engine down every command
still succeeds, the intent is recorded, and the next command restarts it.
Running turns are not its children. v1 already has this process in two
versions (`saddle up` and `saddle plugin engine`) with `up.lock`,
`tui.lock`, `train.lock`, `engine.want` and `mod.alive` to keep them apart.
v2 has it once.

Upgrades: the engine records its build id. A newer binary that finds an
older engine sets `settings.engine_stop`; the engine exits between ticks and
the newer one takes the lock. Turns carry on.

## Data model

```
tasks      id, ticket, title, prompt, role, state, reason, since,
           provider, model, session, branch, worktree, pr, touches (json),
           after (json), fix_attempts, held, created
attempts   id, task, provider, model, argv, resume_argv, pid, started,
           ended, outcome, detail, tokens_in, tokens_out, cost
inbox      id, to_task, from, kind, body, created, delivered_attempt
questions  id, task, body, options (json), asked, answer, answered_by
tickets    forge_id, title, body, approved_by, difficulty, touches,
           after, needs, state, synced
prs        task, number, head_sha, state, checks, mergeable, review,
           observed            -- display cache; decisions re-read the forge
settings   key, value, updated, updated_by    -- autopilot, automerge, …
loops      id, prompt, target, every, until, max_runs, runs, next_at
providers  name, available, out_until, detail
usage      minute, task, provider, model, tokens_in, tokens_out, cost
retries    key, inputs_hash, attempts, next_at, last_error
events     id, ts, task, kind, data           -- audit; never read for state
```

A task is identified by its id for life. A rebase changes a commit's hash
and nothing else about the task.

### Task states

| State | Meaning | Reasons | Exits |
|---|---|---|---|
| `ready` | waiting to start | `slot`, `after:<task>`, `overlap:<task>`, `no_provider`, `held`, `wake` (the orchestrator between turns) | a slot frees, the other task merges, a provider returns, `release`, `kill` |
| `working` | a turn is running or about to, or saddle is publishing its result | `new`, `message`, `unfinished`, `publishing`, `conflict`, `ci`, `check`, `rules` | the turn ends, `kill` |
| `blocked` | waiting on an answer | `question`, `gave_up`, `turn_failed` | `answer`, `retry`, `kill` |
| `pr_open` | published | `checks`, `review`, `queue`, `held` | merged, red or conflicting (to `working`), PR closed, `kill` |
| `merged` | on main (terminal) | | |
| `closed` | ended without merging (terminal) | `killed`, `no_change`, `pr_closed` | `retry` reopens it |

Six states. `fixing`, `landed`, `queued`, `idle`, `parked`, `repairing`,
`needs_you` and `gave_up` are reasons on these, or gone.

## Agents: drivers and turns

A driver is data plus two functions. Three ship (claude, codex, grok) and a
fake for tests.

```go
type Driver interface {
    Name() string
    Probe(ctx) (Info, error)            // installed, signed in, version
    Start(t Turn) Cmd                   // argv, env, files to write first
    Resume(t Turn, session string) Cmd
    Parse(line []byte) []Event          // text, tool, session, usage, error, end
    Classify(exit int, tail []Event) Outcome // ok, quota(reset), auth, crash
}
```

| | claude | codex | grok |
|---|---|---|---|
| start | `claude -p --output-format stream-json` | `codex exec --json --sandbox workspace-write` | `grok -p --output-format streaming-messages-json` |
| resume | `--resume <id>` | `codex exec resume <id>` | `-r <id>` |
| MCP | `--mcp-config <file>` | generated `config.toml` | generated config |
| unattended | allow and deny lists on the command line | sandbox flags | `--allow` / `--deny` |

- **A turn** is one invocation with one prompt. The first prompt is the task
  brief. Later prompts are the task's undelivered inbox rows.
- **A worker finishes by calling the MCP tool `finish`** with
  `status: done | no_change | blocked`, a summary, and for `done` the PR
  title and body. `ask` records a question and ends the turn as blocked. A
  turn that ends without either gets one follow-up turn saying so, then the
  task is `blocked: turn_failed`.
- **Messages arrive at turn boundaries.** To steer a running worker you send
  a message; it is delivered when the current turn ends, or at once with
  `--interrupt`, which stops the turn and resumes the session with it.
- **`saddle attach <task>`** suspends the TUI and opens the real CLI on the
  task's session in its worktree, for a human. Saddle starts no turn for a
  task while someone is attached.
- **Permissions are decided before the turn.** Workers may edit and run
  commands inside their worktree and may not push. The orchestrator may run
  read-only git and `gh`, and may not edit files.
- Each driver has contract tests on recorded streams, and a nightly canary
  runs one real turn per installed CLI, because their formats change.

### The orchestrator

The orchestrator is a task with `role = orchestrator`, no worktree and the
orchestrator toolset. It runs the same way as a worker: the TUI's chat is its
inbox, and each message, question or finished task is a turn.

- **It is disposable.** Every turn's prompt starts with a short digest of
  current state from the database, so a fresh session on another CLI can take
  over at any time: quota failover, `/model`, or a crash. No conversation
  migration, no compaction emulation.
- **Bring your own session.** `saddle connect claude|codex|grok` installs the
  MCP server and a short brief into your own interactive CLI. That session
  then orchestrates with the same tools, and waits with the `wait` tool,
  which returns when something needs it. No bridges, plugin engine or mod.
- **Tools.** `status`, `tickets`, `spawn`, `message`, `answer`, `kill`,
  `retry`, `hold`, `release`, `mode`, `loop`, `ask_owner`, `wait`. Workers
  get `finish`, `ask` and `status`.

### Choosing a provider

One function, `core.Route(task, providers, settings)`, used by every path
that starts a turn. It takes the ticket's difficulty (hard, standard,
mechanical) and `Needs`, a table in config mapping each difficulty to an
ordered list of `provider:model`, and which providers are available. A
provider is marked out when a turn's outcome is `quota`, until the reset the
CLI reported. A task whose provider goes out mid-work resumes on the next
one in its own worktree with its brief and `git log` as context.

## From ticket to main

```
ticket (approved) ─▶ ready ─▶ working ─▶ finish(done)
                                           │ check, rebase, validate
                                           ▼
            merged ◀── forge merges ◀── pr_open
                                           │ red CI or conflict
                                           └────▶ working (same task,
                                                  same branch, same PR)
```

### Intake: approved tickets only

A ticket enters when an open issue carries the approval label
(`saddle:ready`) **and** the label was applied by someone with write access
to the repo, read from the issue's timeline (#462). An issue an outsider
opens, or labels, is never read into a prompt. The orchestrator's `spawn`
with a prompt the owner typed in chat is approved by that. Tickets carry
`Difficulty`, `Touches` (path globs; `Claims` is read as an alias), `After`
and `Needs`.

### Scheduling

A `ready` task starts when a slot is free, a provider is available, every
`after` task is **merged**, and no `working` or `pr_open` task has
overlapping `Touches`. Overlap is advisory and checked only here: no hook
enforces it. A task with no `Touches` is assumed to overlap nothing.

When a worker finishes, saddle records the files its branch changed. If
they overlap another open PR's files, the two are ordered oldest first and
the newer waits (`pr_open: queue`).

### Publishing

On `finish(done)` the engine, in the task's worktree:

1. Runs the repo's check command (`[repo] check`), at most `[limits] checks`
   at once across the machine. Red goes back to the worker as a message.
2. Fetches and rebases the branch onto `origin/main`. A conflict is aborted
   and goes back to the worker, who rebases in its own worktree.
3. Validates the PR title and body the worker wrote against the repo's
   rules (below). A failure goes back to the worker with the rule named.
4. Pushes with `--force-with-lease` and opens the PR, or updates it.
5. If auto-merge is on and the task isn't held, asks the forge to merge when
   ready (`gh pr merge --auto`, which joins the merge queue where the repo
   has one).

### Repo rules

`rules.Load` reads, once per engine start and when the files change: the PR
template, the merge methods the repo allows and its squash title and message
settings, required checks and reviews from branch protection, the commit
convention if the repo declares one, and the attribution rule (from
`AGENTS.md` or config). They reach the worker in its brief, and
`rules.Validate` checks the result deterministically: every template heading
present and in order, no template comment left in, the title matches the
convention, no forbidden trailer or footer.

Because the forge performs the merge, the repo's merge method, its
squash-commit template, required checks, required reviews and CODEOWNERS
apply without saddle modelling them. A PR that waits on a human review sits
in `pr_open: review` and the TUI lists it under "needs you".

### Keeping PRs moving

Each tick the engine reads every open saddle PR in one forge query and, for
each `pr_open` task:

- **Merged**: the task is `merged`; its worktree and local branch are removed.
- **Closed unmerged**: `closed: pr_closed`.
- **Red required check**: `working: ci`, with the failing checks and a log
  excerpt as the message. Checks named in `[ci] flaky` are rerun once first.
- **Conflicting**: `working: conflict`.
- **Behind main and the repo requires up to date**: the oldest green PR only
  is updated (`gh pr update-branch --rebase`). Updating one at a time avoids
  re-running CI on every PR each time main moves. With a merge queue the
  forge does this and saddle does nothing.

After `[merge] fix_attempts` (default 2) fixes on the same PR the task is
`blocked: gave_up`, the PR gets `needs-human`, and the owner is told once.
`saddle retry` or removing the label resets it. If main's own required
checks are red, saddle starts no fixes and says so once.

### What this does about merge conflicts

Parallel work on one codebase cannot be made conflict-free. v2 makes
conflicts rare and never yours:

- Tasks whose declared paths overlap don't run at the same time; the second
  is cut from a main that already has the first.
- Every branch is cut from fresh `origin/main` and every PR targets main, so
  no PR can carry another task's commits and none needs retargeting.
- An undeclared overlap conflicts once, on the newer PR, and that task's own
  agent, which knows the code, fixes it on the same branch.
- Files that are generated (`[merge] regenerate`, for example
  `go.sum = "go mod tidy"`) are regenerated, never merged.

The cost against v1: a task that depends on another waits for it to merge
instead of stacking on it. With auto-merge on, that wait is one CI run.

## Modes

All are rows in `settings`, changed from the TUI, the CLI or the `mode` tool.

- **automerge** (on by default): step 5 of publishing. Off, PRs wait for you.
- **autopilot**: the engine turns approved tickets into tasks while slots
  are free. No model is involved.
- **infinite**: when autopilot has nothing ready and nothing is running, the
  orchestrator is woken to find the next work in the tracker and file
  tickets. Turning infinite mode on is the approval for tickets it files,
  unless `[infinite] approve = "owner"`. It stops at the budget.
- **loops**: `loop "<prompt>" --every 30m [--until merged:#12] [--max 20]`
  queues that prompt to the orchestrator (or a task) on a schedule.
- **budget**: `[budget]` stops starting turns past a share of a provider's
  window, a wall-clock time or a task count. Stopped is a setting, not a
  latch: raise the budget and work resumes.

## TUI

The TUI is a client of the database and the attempt logs.

- One reader goroutine checks `PRAGMA data_version` every 50 ms and, when it
  moved, runs a few indexed, bounded queries and sends the update loop one
  immutable snapshot. It follows the selected attempt's log by size. It
  keeps an event cursor; a gap means re-read everything.
- The update loop handles keys and swaps snapshots. It does no I/O. A user
  action is a row write on a second goroutine.
- Rendering is from the snapshot. Lists render visible rows only. Each row
  and each chat or log line is rendered once and cached by its version and
  the width. Bubble Tea v2 and Lip Gloss v2.
- Budgets, checked in CI by counting operations rather than timing: a
  keypress renders one frame and no query; a frame with 50 tasks and a
  10,000-line log renders only visible rows; first frame needs no network.
- The native CLI is one key away (`attach`) instead of an embedded terminal
  emulator.

The screens are designed separately; see the TUI design canvas.

## Code layout

```
v2/                      module github.com/brandonapol/saddle/v2
  cmd/saddle/
  internal/store/        schema, migrations, every SQL statement
  internal/core/         Transition, Plan, Route: pure, no I/O
  internal/engine/       observe, apply, timers, the lock
  internal/driver/       claude, codex, grok, fake; stream normaliser
  internal/turn/         the turn wrapper
  internal/git/          worktrees, fetch, rebase, push
  internal/forge/        GitHub over gh, fake forge
  internal/rules/        repo rules and PR validation
  internal/tickets/      intake and parsing
  internal/mcp/          the MCP server, both toolsets
  internal/tui/
  internal/cli/
```

v2 lives beside v1 in `v2/` with its own `go.mod` until it can run saddle's
own development. Its binary is `saddle2` and its state is `.saddle2/` until
then, so v1 can build it. At cutover the binary and directory are renamed
and v1's tree is deleted.

## Ideas taken from elsewhere

- **herdr**: the UI is a client of state another process owns; render is
  pure and hidden things cost nothing; a benchmark at 1 and many panes; a
  lost event means re-snapshot; store the resume command per worker.
- **sandcastle, Copilot coding agent**: one invocation per attempt, and
  watch-and-fix on the same PR.
- **Gas Town, workmux, ccmanager**: a provider is a small table of command,
  flags and capabilities; a setup step for fresh worktrees.
- **Merge-queue practice**: the queue owns the rebase; serialise only on
  overlapping paths; identity belongs to the task, not the commit.

## Open questions for the owner

1. No stacked PRs in 2.0: `after` waits for the merge. Acceptable?
2. Infinite mode files and approves its own tickets by default. Or `owner`?
3. GitHub only at 2.0 (the forge interface stays). GitLab later?
4. Gemini and opencode drivers are out of 2.0. Wanted sooner?
