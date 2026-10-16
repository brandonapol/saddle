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
  queues as `t0` notices. On Claude Code 2.1.287 or newer the plugin's mod
  takes them (`saddle plugin events --json`), submits a `[saddle]` prompt
  for the ones that need a decision and shows the rest; its heartbeat
  (`.saddle/mod.alive`) keeps the other two paths off. Without the mod,
  `saddle plugin wait`, run in the background, exits when one needs the
  session, and `saddle plugin hook` delivers them on the session's prompts
  and tool calls. The engine and `saddle up` share
  `.saddle/tui.lock`, which records its holder, so only one of them drives
  `t0`. The plugin's hook and MCP server do nothing outside a repo that ran
  `saddle init` or inside saddle's own agents (`SADDLE_TASK` set).

Concurrent processes coordinate through SQLite transactions, not a socket.
Scheduling decisions are made in-process, inline in `internal/app`. The
reactor (#45) is descoped and closed, because the daemon it was designed for
was dropped (#5). The decision record is `docs/backlog-audit.md`. The git
watcher and dispatcher described below are the target design.

## The reactor (event loop)

Descoped and closed (#45). See `docs/backlog-audit.md`.

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

### Done and its background gate (#368)

A worker's `done` must pass the repo's own gate (`[train] lint.cmd`, or the
detected pre-commit gate such as `make check`) before it is queued. Run inside
the MCP call, that gate could keep the call silent for over 30 minutes on a
busy machine; Claude Code then gave up and retried, and checks piled up. So
the MCP `done` tool calls `App.DoneAsync` (`internal/app/done.go`):

1. The quick checks run inline: a worker, a clean worktree, commits to land.
   A tree that already passed the gate (its stamp in
   `<git common dir>/saddle-hooks`) is queued at once, as before.
2. Otherwise done takes the task's job lock (`saddle-hooks/done-<task>.lock`,
   a non-blocking flock that also holds the job id), starts the gate in a
   goroutine and returns the job id right away.
3. A `done` call while that lock is held starts nothing: it joins the
   running job and gets its id back. The lock dies with its process, so a
   server that went away mid-gate never blocks the next done.
4. The job runs the gate under the repo hooks' lock, so gates from every
   worktree of the repo run one at a time. When it passes, and HEAD's tree
   is still the one it checked, the task is marked done and queued and the
   agent gets an info notice. A red gate, an interrupt or a branch that
   moved meanwhile sends the agent an action notice (which wakes it) with
   the reason and the job id.

`saddle mcp` waits for its running jobs before it exits, so a client that
hangs up mid-gate still gets its task queued. The CLI's `saddle done` still
runs the gate inline.

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
the bottom). Unrelated work gets separate stacks. A file that three or more
stacked tasks change (docs, registries, central config) is a hub and doesn't
link them on its own. A task whose commits don't replay without other work
stacks on the latest earlier task that changed the same files, not on
whatever landed just before it (#226). Saddle restacks a stack when
a lower layer changes and retargets it when a lower layer merges.

**Dependencies stack whatever the file overlap (#193, #358).** A task stacks
on the earlier tasks it runs after, and those edges (`.saddle/deps/<task>`,
each logged as a `depends_on` event) come from three places: `after` on
spawn; the task's title, prompt or issue naming an earlier stacked task's id,
issue, PR or branch ("follow-up to #3093"), which `prs` and `restack` read
before laying out the stack; and a PR someone retargeted by hand (below). A
layer whose dependency is out of its stack but still has an open PR (in the
merge queue, say) is left where it is rather than moved onto base without
it; once that PR merges, the orchestrator hears which layers built on it and
the next restack moves them onto base. The pre-publish gate checks each
layer at its PR head on its PR's own base, in `prs` and in `restack`, never
at the integration tip, which holds other stacks' work too: a red head is
neither published nor pushed.

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
- **Manual retargets stick (#359).** Saddle remembers the base it last set
  on each PR (`.saddle/pr-bases.json`). A base GitHub reports that saddle
  didn't set, on the branch of a stacked task that landed earlier, is kept:
  it becomes a recorded dependency, so `restack` and `prs` stack the PR there.
  A base on the branch of a saddle task outside the stack is a dependency too:
  the PR stays there until that task's PR merges. When GitHub can't say where
  a PR points, `restack` doesn't re-set a base it already set. Saddle never
  changes a base silently: each change from what GitHub had is a
  `pr_base` event naming the old and new base, and undoing a hand-set base
  (onto main, a later layer or a branch saddle doesn't know) also tells the
  orchestrator why and how to keep tasks together (`saddle stack create`).
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
- **PR bodies follow the repo's template (#361).** Both `prs` and `publish`
  lay the description into `.github/pull_request_template.md` (or a variant)
  strictly (`internal/app/prtemplate.go`): every heading kept in order,
  `Closes #N`-style placeholders filled or dropped, prompt comments removed,
  the Changes section listed from commit subjects, and type, surface and
  automated-test boxes ticked from the commits and changed paths. Boxes saddle
  can't vouch for, like manual testing, stay unticked. A trailing AI
  attribution footer on the description stays last. With no template, `prs`
  sends the task summary as before.

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
  hookless, so claims are checked at commit and done (#489) and notices are
  typed in. Hookless launches export `SADDLE_HOOKLESS=1`; the pre-commit
  hook saddle installs beside the ref guard then runs `saddle refguard
  pre-commit`, which puts each staged file through the same claim check as
  a hooked write, and done refuses a branch that changes another task's
  files. `SADDLE_ALLOW_CLAIMED=1` overrides either, recorded as a
  `claims_overridden` event and reported to the orchestrator. Usage and status
  come from their output on a best-effort basis: a hookless worker's screen
  marks it idle or running, and saddle types codex and grok on after a
  usage-limit reset. `docs/adapter-parity.md` lists every feature per adapter.

### Providers, per-task choice and rotation (#321)

Three providers back the adapters: Anthropic (`claude`), SpaceXAI (`grok`)
and OpenAI (`codex`). The orchestrator brief lists them with their models and
strengths and tells the orchestrator to pick per task: claude for hard design,
cross-package refactors and review; grok for fast, well-scoped work and a
second opinion; codex for well-specified or mechanical edits whose claims no
one else touches (it has no hooks). `status` shows each adapter's `provider`,
`hooks` and, while it is out, `out_of_quota_until`.

A spawn with no `adapter` is picked by one chooser (#481, #505,
`internal/app/adapterpick.go`) that the orchestrator's spawns, autopilot and
fallback rerouting share. Only adapters with quota that can run here count.
A spawn's `needs` (capabilities from the ticket's `Needs:` line) puts
adapters that declare them first, then its `difficulty` (hard, standard,
mechanical) puts adapters whose capability table suits it
(`[adapters.<name>] difficulties`, `capabilities`, `models`; defaults in
`internal/config/routing.go`), then its `kind` puts adapters whose
`[adapters.<name>] kinds` list it; a task with claims then prefers adapters
that run hooks (under `balanced`, only when its claims share a directory
with another live task's). Then the policy decides: `balanced` (the default)
takes the most unused budget per running worker, so work spreads across
providers; `order` takes
the session's harness (`saddle up claude|grok|codex`, or `harness`) and then
`[adapters] order`, and never trades the harness for the hooks preference;
`cost` takes the lowest `[adapters.<name>] cost` (unset is 1); `quota` takes
the most of its five-hour `budget` left, from the metered usage (no budget
means no known limit). `status` shows `adapter_policy` and each adapter's
`pick_rank` and `pick` (why), and `saddle status` prints the order as "next
spawn:"; each task's `pick` in status says where it went and why. That holds for every path saddle spawns on its own: CI fix and
CI-red repair tasks, stuck-task repairs, autopilot top-ups, and resumes of
tasks whose run dir recorded no adapter. A model from another provider's
family (a ticket's `opus`, a repair's `sonnet`) falls back to the chosen
adapter's default.

`[adapters.<name>] exclude_models` lists models saddle never runs workers on
(#482). Unset, codex excludes `astra`, which burns too many tokens; `[]`
excludes nothing. An entry matches the model id or one of its words, so
`astra` covers `gpt-6-astra`. A spawn naming an excluded model is refused,
even with force, with a message naming the config key; a handoff or resume
carrying one falls back to the adapter's default (or the CLI's own when that
is excluded too). The orchestrator brief lists the exclusions, `status` gives
each adapter's `excluded_models`, and `saddle status` prints them.

When a worker's screen shows a usage-limit banner (Claude Code's, Codex's
"usage limit" or an OpenAI/Grok out-of-quota error; `usage.DetectLimitBanner`),
the engine or the TUI parks it as in #180 and marks its adapter out of quota
until the banner's reset (or `app.ExhaustFor`, one hour, when it names none).
The state lives in `.saddle/adapters-exhausted.json`. Spawns that would use
that adapter go to the next one in `[adapters] order` (default `["claude",
"grok", "codex"]`) that has quota and can run here; an explicit `adapter=` on
an exhausted one is refused unless forced. A lost or paused task whose adapter
is out resumes on the next one with its brief. A task running on it when it
runs out is handed off (#481, `internal/app/handoff.go`, `[adapters]
failover = "handoff"`, the default): saddle snapshots its work, closes its
window and launches the best adapter with quota (a hooked one when it claims
paths) in the same worktree and branch, its prompt plus a note that it is
taking over and should start from `git status` and the branch's log. The
orchestrator gets a digest line. It parks as before (and continues after the
reset) with `failover = "park"`, when the reset is within `handoff_wait`
(15m), or when no other adapter has quota. When the reset passes, or a
parked worker's banner clears, the adapter is restored. With every adapter
out, spawn returns a paused-launch error (autopilot and infinite mode sleep
until the first reset).

`[fallback]` chains (#483, `internal/app/fallback.go`) refine this per
model: `"codex:sol" = ["claude:opus", "grok"]` makes a sol worker's limit
mark only `codex:sol` out (an entry keyed `adapter:model` in
`adapters-exhausted.json`), so codex's other models stay usable. Spawns that
would run sol, its handed-off workers and its lost tasks go to the first chain
entry that has quota, can run here and isn't in `exclude_models`; with none,
`[adapters] policy` picks among the other adapters. An adapter key (`"grok" =
["claude"]`) goes ahead of the policy while that adapter is out. A model with
no key is tracked with its adapter, as before. Status shows each chain
(`fallback`) and each adapter's `models_out_of_quota`.

The TUI's orchestrator fails over too (#481, `internal/app/orchfailover.go`).
When a turn fails, retries or exits on a quota error, saddle marks its
adapter out and restarts the orchestrator on the next adapter with a headless
session (claude or grok; codex has none), recorded in
`.saddle/orchestrator-adapter`. A conversation can't move across CLIs, so
the new session's first message is a state summary: the headline, the live
tasks with their adapters, the last two hours' catch-up and the end of the
old conversation. Its brief, status identity and transcript follow it. The
next `saddle up` goes back to the default once that has quota again. The
plugin orchestrator is the owner's own session, which saddle can't restart.

Each rotation notifies the owner once: a footer banner in the TUI, an action
notice to the orchestrator naming who ran out, who takes over and when it
resets (plus a warning when the successor has no hooks, so claims become
advisory), and, when `[notify]` sets them, a desktop notification
(`notify-send`, or `osascript` on macOS) and a JSON `{"text": ...}` POST to
`webhook`. Every adapter running out sends a distinct notice naming the first
reset. A restored adapter is routine news: a digest line, no desktop ping.

Sub-agents spawned through MCP are ordinary tasks with a `parent_task`. Each
gets its own worktree, window and claims, and the train lands it before its
parent. Spawn depth and fan-out are capped.

The orchestrator can run as a hierarchical advisor (`[claude.advisor]`, #257),
off by default. When on, the lead model (`sonnet`) drives the session at high
effort, Claude Code subagents (`haiku`, via `CLAUDE_CODE_SUBAGENT_MODEL`) swarm
read-only lookups, and `claude --advisor opus` keeps the advisor on call. The
brief allows it at three checkpoints only: before a plan locks, when the same
failure repeats, and before done. Opus is never the synchronizer. Saddle
probes each flag before launch and fails, naming the flag, if `claude`
rejects it. Workers are unaffected.

## Narrator and usage

The narrator (`claude-haiku-4-5`) reads compact event deltas, never raw pane
text unless you ask a question about a window. It writes one line per salient
change into a single thread. Usage comes from each adapter's durable local
record: Claude Code transcript JSONL, Codex rollout JSONL, and Grok session
`usage.json` ledgers. It is bucketed per minute by task, adapter and model;
provider-reported cost wins when available, otherwise model pricing supplies
the estimate. The TUI graph and rows are per provider, including an observed
quota reset when adapter rotation has marked one exhausted. Codex is split by
model (#482): each rollout `turn_context` names the model of the token counts
after it, and luna, terra, sol and astra each get a row and bar colour of
their own; any other codex model gets its own labelled row, and a model in
`exclude_models` says "excluded". Plan-limit bars
remain estimates against caps you configure.

## Heavy-run queue

`saddle run --class C -- cmd` (`internal/runq`, glue in `internal/app/runq.go`)
makes CPU-heavy commands take turns across every agent, session and repo on
the machine. The queue is a SQLite file in `$XDG_STATE_HOME/saddle/runq.db`.
Each lease holds an flock, so a dead holder's slot is reaped on the next poll.
`SADDLE_RUNQ_LEASE` lets nested runs ride the outer lease. Config comes from
`.saddle/runq.toml` with `~/.config/saddle/runq.toml` on top. The default mode
is `observe`: runs are recorded with wait, duration, CPU and peak RSS, but none
waits. `App.RunHeavy` is the entry point for saddle's own gates. Design:
[runq.md](runq.md).

## Scratch root, sweeper and spawn hold (#322)

Saddle keeps its temp files under one root, `[tmp] dir`, which defaults to
`~/.cache/saddle/tmp`. It is read through `internal/config` like any other
section. `saddle up`, `mcp`, `spawn` and `land` point `TMPDIR` and `GOTMPDIR`
there, so gates (in per-run `<root>/<repo hash>/gate-*` dirs), the train and
agents inherit it. Each agent also gets its own `<root>/<repo hash>/task-<id>`
dir as `TMPDIR`, `GOTMPDIR` and `CLAUDE_CODE_TMPDIR`. Kill and land remove it;
done keeps it, and the sweep never removes it while the task is live.
`internal/scratch` sweeps entries under the root, plus
saddle-prefixed entries in the OS temp dir, that are older than `max_age` and
held by no live process. It runs on `up`, every 10 minutes while `up` runs,
and after done or kill. Before each spawn and on each sweep tick,
`App.ScratchPressure` measures the root's disk, capped by `max_bytes`. When it
is under `low_free_pct`, saddle sweeps again with a 5-minute threshold. If the
disk is still low, it holds spawns: spawn fails with `ErrScratchLow`, the
orchestrator gets a notice naming the biggest entries, and status shows a
warning. The hold lifts once there is room, and spawn's `force` overrides it.
Design: [scratch.md](scratch.md).

## SRE mode (#547)

`internal/sre` holds SRE mode; `internal/cli/sre*.go` and the hook are thin
layers over it. Details: [sre.md](sre.md).

- **Config** (`config.go`): `.saddle/sre.toml` parses into checks, endpoints
  and credentials. Unknown keys are errors. Credentials must be an env var
  name or a `scheme:value` ref; plaintext secrets fail validation. `saddle sre
  check-config` runs it.
- **Intake** (`intake.go`, `lanes.go`, `plan.go`): `saddle sre` parses the
  pasted text into a summary and the source paths it names, runs each check's
  `command` read-only through a `Runner` (a check whose credential env var is
  unset is skipped; a `surface` is left for an investigator; output is
  redacted), then plans lanes. `code` and `logs` always run; `infra` and
  `deps` are added when the incident text or a failed check points there,
  and `infra` is the third lane when nothing does. Only the code
  lane claims paths (the named files that exist, capped at 20). `Launch`
  refuses overlapping claims before spawning anything.
- **Guard** (`guard.go`, `guard_store.go`, `internal/hook/sre.go`): intake
  opens an incident in `.saddle/sre-incident.json` under the main checkout,
  then reserves each investigator's task id in it before spawning, so the
  guard holds it from its first tool call (#567). The PreToolUse rule
  `sre-guard` lets read-only calls through and denies the rest with
  `ErrLocked` until a root cause stated by one agent is verified by another.
  After that a mutating call is filed in the decision queue (class
  `sre-action`, irreversible, "Hold" recommended) and runs only if the owner
  picks "Run it". An unreadable incident file denies mutating calls, and the
  denial says how to clear it. `saddle sre close` lifts the guard.
- **Postmortem** (`postmortem.go`): `WritePostmortem` renders a
  `PostmortemRecord` to `docs/postmortems/<date>-<slug>.md` with `O_EXCL`,
  adding `-2`, `-3`, ... rather than overwrite. Checks that found the cause
  become proposed runbook additions in the file; no runbook is written.
  `saddle sre close` builds the record from the incident
  (`PostmortemFromIncident`) and writes it. Format: [postmortems/README.md](postmortems/README.md).

## Forge layer (#644, #647)

Saddle reaches its git host through `internal/forge` and nowhere else
(decision: [ADR 0001](adr/0001-forge-layer.md)).

- **One process start.** `forge.Output`, `forge.CombinedOutput` and
  `forge.CLI` are the only code that runs `gh` (and, with #645, `glab`).
  `TestNoDirectCLIExec` parses the repo and fails when another package passes
  a forge CLI to `exec.Command`. Generic exec helpers (doctor's `Env.Exec`,
  the changelog's runner) hand a forge CLI name to `forge`.
- **One interface.** `forge.Forge` is the provider-neutral surface: create,
  view, list, retarget, edit, merge and close PRs; comments and labels; CI
  checks, failed-job logs and reruns; merge settings; auth; issues and
  sub-issues. Its types (`forge.PR`, `forge.CheckRun`, `forge.CICheck`,
  `forge.Issue`, …) carry no GitHub JSON names. `forge.GitHub` implements it over `gh` with the same
  arguments saddle always passed. Operations a provider lacks return
  `forge.ErrUnsupported`, and saddle drops the feature instead of failing.
- **Selection.** `App.Forge()` is `forge.New(cfg.Forge, root)`: GitHub unless
  `[forge]` says `cli = "none"` (local-only) or GitLab (not supported until
  #645). It starts no process; detecting the provider and CLI from the
  remote is a setup step in `saddle init` and `saddle doctor`
  (`doctor.DetectForge`, #646).
- **Every caller.** `ciwatch`, `sweeper`, `automerge`, `sentinel` (the stack
  sentinel and the ci-red watcher) and `planner.Push` each declare the small
  slice of `forge.Forge` they use and are built from `App.Forge()`, so
  `[forge]` applies to them too. On `ErrUnsupported` each degrades quietly:
  the CI watchers watch nothing, labels and stack comments are skipped, and
  auto-merge leaves its PRs unread. `TestNoGHArgsOutsideForge` fails when a
  package outside `internal/forge` builds gh arguments; its allowlist names
  the GitHub-only exceptions: `ghstack` (the gh-stack extension), `doctor`
  (setup-time diagnostics) and the changelog.
- **Tests.** `internal/e2e/fakegh` is a fake `gh` binary on `PATH`, so e2e
  journeys exercise `forge.GitHub` end to end.

## Layout on disk

```
<repo>/.saddle/
  config.toml        per-repo config (merged over ~/.config/saddle/config.toml)
  runq.toml          heavy-run classes and mode (under ~/.config/saddle/runq.toml)
  state.db           sqlite: epics, tasks, deps, claims, events, usage, train, renames
  sre.toml           SRE mode checks, endpoints and credential refs
  sre-incident.json  the current (or last) SRE incident and its guard state
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
internal/forge/      the git host: forge.Forge, GitHub over gh, the only gh exec
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
internal/runq/       heavy-run lease queue behind saddle run
internal/sre/        SRE mode: sre.toml, intake and lanes, read-only guard, postmortems
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
6. Decisions are made in-process. The reactor (#45) is descoped and closed,
   because the daemon it was designed for was dropped (#5). See
   `docs/backlog-audit.md`. Git watcher (#46), then the forecaster and
   dispatcher.

### Operator recovery of guarded refs

From a normal shell outside a Saddle task, use `SADDLE_REFGUARD=off git
update-ref -m 'operator recovery' <ref> <new> <old>` to repair a branch, or
`SADDLE_REFGUARD=off git push ...` to publish a repair. The guard records each
protected ref update or push with actor `operator` in `.saddle/state.db`, which
must be available. Task processes with `SADDLE_TASK` set cannot use this override.
The repository's chained hooks still run. The denial message and `saddle doctor`
both show this recovery path; deleting the hooks is unnecessary.
