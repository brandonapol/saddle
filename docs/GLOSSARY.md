# Saddle glossary

Every Saddle-specific term, A to Z: CLI commands, task and train states,
stack flags, config concepts, adapters, autopilot and heavy runs. Each entry
says what the thing is, shows a command or a status line, and links to the
deeper doc or issue. Commands are listed by name without the `saddle`
prefix, so `saddle land` sits under L.

New here? Read [How Saddle works](../README.md#how-saddle-works) first. It
walks the lifecycle once:

```
spawn -> work -> done -> land -> prs -> merge
```

`internal/docs` tests this file against the code: a CLI command, task
state, train state or MCP tool with no entry here fails `make check`.

Two kinds of state show up in `saddle status`. The **STATUS** column is the
task's status (what its agent is doing). The **TRAIN** column is its merge
train entry (where its branch is on the way to a PR). Both are listed here.

### `adapter`

The agent CLI a task runs on: `claude`, `codex`, `gemini` or `grok`. Claude
and Grok workers run Saddle's hooks, so their claims are enforced; Codex and
Gemini have none, so their claims are advisory. When one adapter runs out of
quota, new spawns rotate to the next in `[adapters] order`.

Example: `spawn` with `adapter=codex`; `saddle status` lists each adapter's
`provider`, `hooks` and `out_of_quota_until`.
See [Providers, per-task choice and rotation](ARCHITECTURE.md#providers-per-task-choice-and-rotation-321).

### `at-risk` flag

The [stack sentinel](#stack-sentinel)'s mark on a broken PR stack (a PR
GitHub can't merge, base moved past integration, a drifted branch). While
it is up, `prs` publishes only the layers below the broken one and `land`
holds queued work that touches the broken layers' files. Only a real
conflict also labels PRs `needs-human`. [`restack`](#restack) is the fix;
`saddle sentinel ack` acknowledges it when restack can't.

Example: `stack_at_risk` in `saddle status`.
See [Stack upkeep](ARCHITECTURE.md#stack-upkeep-when-people-use-github-119-123), #119.

### `saddle automerge`

Auto-merge of ready PR stacks, off by default. When on (and `saddle up` or
the plugin engine is running), it merges the bottom PR of a stack whose
checks are green, mergeable and CLEAN, not a draft, not `needs-human` and
not at risk, then restacks and repeats. Any failed merge stops it until it
is turned on again. Subcommands: `saddle automerge on`, `saddle automerge
off`, `saddle automerge status`, `saddle automerge hold <stack>` (never
merge this stack, keep restacking it) and `saddle automerge release
<stack>`. MCP tool: `automerge`. Config: `[train] auto_merge`.

Example: `saddle automerge status --json`.
See #146, #152.

### `saddle autopilot`

Lets Saddle drive the loop itself every 30 seconds while `saddle up` runs:
resume tasks that lost their agent, land what's queued and publish PRs, top
up to the concurrency cap from open issues labelled `saddle:ready` (oldest
first, honouring `after: #N` lines and disjoint `claims:` globs), and nudge
an idle orchestrator when work stalls. It never merges; that stays with
`saddle automerge`. Subcommands: `saddle autopilot on [--until 18:00]
[--until-usage 90%] [--max-tasks N] [--ready-label L]`, `saddle autopilot
off`, `saddle autopilot pause`, `saddle autopilot resume`, `saddle autopilot
status` and `saddle autopilot infinite on|off` ([infinite mode](#infinite-mode)).
MCP tool: `autopilot`.

Example: `saddle autopilot on --until-usage 90%`.
See #285.

### barrier

A task that moves or renames directories, or restructures enough that
everything else depends on it. The planner runs it alone in wave 0; the
orchestrator lands it before starting the work that builds on it.

Example: a plan's `barrier` route in `saddle plan show`.
See [Collision handling](ARCHITECTURE.md#collision-handling-from-cheapest-to-most-expensive).

### base

The branch PRs finally merge into, `main` unless `base` in
`.saddle/config.toml` says otherwise. Restack rebuilds the stack on
`origin/<base>`; `publish` opens its PR against it.

Example: `base = "main"`.
See [Merge train](ARCHITECTURE.md#merge-train).

### `saddle brief`

Prints one task's brief: its goal, the paths it owns, the paths other tasks
own (hands off), the done-when checklist and the tasks it spawned. With no
task it uses `SADDLE_TASK` or the current worktree. Agents read the same
thing through the `brief` MCP tool. The orchestrator has a brief too:
`saddle plugin brief`.

Example: `saddle brief t3 --watch`.
See [Agents](ARCHITECTURE.md#agents).

### checkpoint

A snapshot of a live worker's working tree, untracked files included, saved
to `refs/saddle/checkpoints/<task>` on a timer without moving the agent's
index, HEAD or branch. Checkpoints are never pushed and are pruned when the
task lands or is killed. `saddle kill` and `saddle down` also save
uncommitted work to `refs/saddle/wip/<task>`. The same watcher nudges agents
to commit coherent units.

Example: `git log refs/saddle/checkpoints/t3`.
See #50.

### `ci-red`

The label and hold on a stacked PR whose CI checks failed on its head. That
layer and every layer above it in its PR stack are held: `prs` doesn't push
them, `land` holds queued work that changes their files, and auto-merge
merges none of them. Saddle spawns a repair task; the hold lifts by itself
once the checks pass. `saddle sentinel ack` acknowledges it.

Example: a PR labelled `ci-red`; `land` notes `held: ... it would stack on red CI`.
See #213.

### `saddle claim`

Reserves paths (globs like `internal/foo/**`) for a task. A Claude Code
PreToolUse hook denies writes to paths another live task has claimed, and
the denial names the owner. Unclaimed files are claimed on first write.
Claims are released when a task lands or is killed, or with `saddle
release`. MCP tool: `claim`; to ask an owner for a change, `ask_owner`.

Example: `saddle claim 'internal/docs/**' --task t3`.
See [Collision handling](ARCHITECTURE.md#collision-handling-from-cheapest-to-most-expensive).

### `saddle completion`

Cobra's generated shell completion script.

Example: `saddle completion zsh > "${fpath[1]}/_saddle"`.

### `saddle concurrency`

Shows how many worker agents are running and the cap, or changes the cap
(1 to 16) at runtime until `saddle concurrency reset`. Lowering it stops
nothing; spawn waits until fewer run. MCP tool: `concurrency`.

Example: `saddle concurrency 3`.
See `concurrency` in `.saddle/config.toml`.

### `conflict`

Both a task status and a train state: the merge train sent the branch back
because it didn't rebase onto integration, a regen failed, or its worktree
was dirty. The agent runs `saddle sync`, resolves and calls `done` again.
After `[train] max_attempts` failures the entry is [`escalated`](#escalated)
instead.

Example: `t21  conflict  ...  conflict: rebase failed`.
See [Merge train](ARCHITECTURE.md#merge-train).

### `saddle doctor`

Preflight checks for the repo, `gh`, tools and hooks, grouped by who fixes
them, each with its fix. `--fix` applies the local ones.

Example: `saddle doctor --fix`.
See [QUICKSTART](QUICKSTART.md).

### `saddle done`

A worker's "I'm finished": it needs a clean, committed worktree with at least
one commit beyond integration, runs the repo's lint gate, stores the summary
(which becomes the PR description), sets the task to `done` and queues the
branch in the merge train. It doesn't land anything. MCP tool: `done`.

Example: `saddle done -s "Add the glossary and its drift test."`.
See [Merge train](ARCHITECTURE.md#merge-train).

### `done` (task status)

The task called `done` and its branch is waiting in the merge train
(train state `queued`).

Example: `t7  done  opus  ...  queued`.

### `saddle down`

Stops every agent. Running tasks become [`paused`](#paused): their work is
snapshotted and their claims, worktrees and branches kept, and `saddle up`
resumes them.

Example: `saddle down`.
See #254.

### epic

A body of work bigger than one task, usually a GitHub issue with
sub-issues. `saddle plan` turns one into a task graph; `saddle up <epic>`
starts with it.

Example: `saddle plan gh:#44`.
See [docs/QUICKSTART.md](QUICKSTART.md).

### `escalated`

A train state. A branch that failed to land `[train] max_attempts` times
(default 2) goes to the owner as `needs_you` instead of back to its agent,
so nobody loops on the same failure. Calling `done` again queues it once
more.

Example: `escalated after 2 failed attempts`.
See #30.

### `failed`

A task status: its spawn failed. The row stays, with the reason in its
summary, so the id is never reused.

Example: `t40  failed  ...`.

### `folded`

A train state for a CI repair task whose commits joined the red layer it
fixed. It leaves the stack and never gets a PR of its own.

See #213.

### `saddle gc`

Removes worktrees, `saddle/*` branches (local and remote) and `refs/saddle/*`
refs no live task uses, once their work is merged (directly, squashed or
rebased). Unmerged work, branches of PRs still in the stack and dirty
worktrees are listed with the reason and kept.

Example: `saddle gc --dry-run`.

### harness

The agent CLI the session runs on by default: `claude` (the default),
`grok` or `codex`. It is used for the orchestrator and for every spawn that
doesn't name an [adapter](#adapter), including Saddle's own repair and
autopilot spawns.

Example: `harness = "grok"` in config, or `saddle up grok` for one run.
See [Providers](ARCHITECTURE.md#providers-per-task-choice-and-rotation-321), #321.

### heavy run

A CPU-heavy command (tests, linters, e2e suites) run through `saddle run`,
so parallel agents take turns on one per-machine queue instead of saturating
the box. Each run belongs to a class (`go-test`, `golangci-lint`, `e2e`,
...) with a number of slots, and holds a lease while it runs. Modes:
`observe` (record, never wait; the default), `enforce` and `off`.

Example: `queued: position 1 of 2 for go-test, holder t83 (make check, 40s), ~2m`.
See [docs/runq.md](runq.md).

### `held`

A `land` result, not a stored state: the queued branch changes files that an
at-risk or ci-red layer changed, so it waits, still `queued`, until the
stack checks clean or goes green.

Example: `held: it changes go.mod, which the at-risk part of the stack changed`.
See #119, #213.

### `saddle help`

Lists every command; `saddle <command> --help` explains one.

Example: `saddle help stack`.

### `idle`

A task status: the agent stopped and is waiting at its prompt. Action
notices wake it.

Example: `t0  idle  sonnet  ...  orchestrator`.

### infinite mode

Autopilot with no stop condition. When the ready queue is empty it asks the
orchestrator to find more work; when a plan-limit window is full it
[parks](#park) the running tasks and resumes them on reset. Holds, ci-red
and the sentinels still apply. The TUI's `alt+i`.

Example: `saddle autopilot infinite on`.
See #285.

### `saddle init`

Creates `.saddle/` with a config template, adds it to git's exclude file,
and installs the ref guard hooks, after asking whether you trust the repo.

Example: `saddle init`.
See [QUICKSTART](QUICKSTART.md).

### integration branch (`saddle/integration`)

The branch the merge train lands onto, one task at a time. Only the train
moves it (the ref guard denies anyone else). Task branches are cut from it
and `saddle sync` rebases onto it. It is local until `prs` or `restack`
pushes work from it.

Example: `integration = "saddle/integration"`.
See [Merge train](ARCHITECTURE.md#merge-train).

### `saddle kill`

Stops a task's agent and releases its claims. Uncommitted work is saved to
`refs/saddle/wip/<task>`, the worktree is removed, and the branch too when it
has no commits beyond integration. A branch with commits, or a dirty
worktree, is always kept. `--keep` keeps both. MCP tool: `kill`.

Example: `saddle kill t9`.

### `killed`

A task status: the task was stopped with `kill` or `rescue`. It holds no
claims. A killed task's landed work leaves the PR stack
([`superseded`](#superseded)).

Example: `t12  killed  opus  ...`.

### `saddle land`

Runs the merge train. Each queued branch, in queue order, is rebased onto
the integration branch, tested with `[test] cmd` and the lint gate, and
fast-forwarded in. A failure goes back to the agent that wrote the branch
and the train moves on. Land never pushes, never opens PRs and never
touches `main`; that's `prs`. It skips `on_hold` entries and holds work
that touches an at-risk or ci-red layer. MCP tool: `land`; TUI key `L`.

Example: `saddle land` → `t7 landed`, `t8 conflict: rebase failed`.
See [Merge train](ARCHITECTURE.md#merge-train).

### `landed`

A task status and a train state: the branch is on the integration branch,
its claims are released, and (with `close_on_land`) its window and worktree
are gone. Its train note keeps the range it landed. It stays in the PR
stack until its PR merges ([`merged`](#merged)) or it leaves
([`superseded`](#superseded)).

Example: `t5  landed  opus  ...  landed: 2771bc9..2df4561`.

### layer

One landed task's commits as a PR in a stack. Each layer's PR targets the
branch of the layer below it, or base for the bottom one.

See [Merge train](ARCHITECTURE.md#merge-train).

### lint gate

The repo's own pre-commit or lint check (`make check`, a pre-commit hook,
lefthook, husky, ...). `done` runs it in the worktree, and the train runs it
after the tests. Red goes back to the agent like a red test. Saddle detects
it; `[train] lint.cmd` overrides it and `""` turns it off.

Example: `lint.cmd = "make check"`.

### merge train

Saddle's serial landing queue: `done` queues a branch, `land` lands queued
branches one at a time (rebase → test → fast-forward) on the integration
branch. Only the train moves the integration branch or pushes saddle's
branches. See `saddle queue` to reorder or hold entries.

Example: `saddle queue`.
See [Merge train](ARCHITECTURE.md#merge-train).

### `merged`

A train state: base has the task's work, because its PR merged (merge,
squash or rebase) or restack found the work there. The task has left the
stack for good.

Example: `t1  landed  ...  merged: d89d45a..7794e63`.
See #119.

### `saddle message`

Sends a `[saddle]` message to a task, waking it if it's idle. MCP tool:
`message`. The orchestrator also reads a task's screen with `peek` and
answers prompts with `send_keys`.

Example: `saddle message t3 "rebase onto integration and retry"`.

### narrator

A cheap model (Haiku) that reads the event log and writes one plain-English
line per salient change, so you know what every window is doing.

See [Narrator and usage](ARCHITECTURE.md#narrator-and-usage).

### `needs-human`

A GitHub label the stack sentinel puts on PRs only for a conflict that
needs a decision, with a comment naming the conflict. It comes off when the
stack checks clean or the flag is acknowledged.

See #119.

### `needs_you`

A task status (▲ in the TUI): the agent asked for permission or input, or
the train escalated its branch. Saddle hands the screen to the orchestrator,
which answers if that's safe or asks you.

Example: `t4  needs_you  opus  ...`.

### notice

A message from Saddle to an agent. Action notices (failed tests, conflicts,
messages) wake an idle agent; info notices ride along on its next tool
call. For the orchestrator, only questions and real decisions interrupt;
routine news (landed, merged, restacked, CI green) is rolled into a digest.

See `[notices]` in config, #222.

### `saddle notices`

Prints the orchestrator's pending digest; `--all` lists every orchestrator
notice with how it was routed (interrupt, digest, silent).

Example: `saddle notices --all`.
See #222.

### `on_hold`

A train state: a queued entry the owner held back with `saddle queue hold`.
`land` skips it, it keeps its place, and `done` doesn't release it.

Example: `saddle queue release t8`.
See #146.

### orchestrator

The chat agent (task `t0`) that plans work, spawns workers, answers their
prompts and runs the train. It is a headless Sonnet session in `saddle up`,
or your own Claude Code session through the plugin. It doesn't write code.

See [Processes](ARCHITECTURE.md#processes).

### `orphaned`

A status shown, never stored, for a live task whose window is gone and
whose agent has been silent for 10 minutes. `saddle resume` gives it a new
window; `saddle up` does that by itself.

Example: `t6  orphaned  ...`.

### park

Stopping running workers the way `saddle down` does (work snapshotted,
claims and worktrees kept, status `paused`) to wait out a plan-limit or
quota reset, then resuming them. Infinite mode and adapter rotation park
tasks; they continue on their own after the reset.

See #180, #321.

### `paused`

A task status: `saddle down`, a park, or SIGTERM on `saddle up` stopped the
agent. It keeps its claims and worktree, and `saddle up` resumes it.

Example: `t3  paused  opus  ...`.
See #254.

### `saddle perf`

Times the reads the TUI and orchestrator poll and counts the state that has
piled up (tasks by status, table rows, what gc would remove).

Example: `saddle perf --runs 10`.

### `saddle plan`

Plans an epic into tasks with the planner model and saves the plan as TOML
under `.saddle/plans/`. `saddle plan show` prints it with the checker's
waves, train routes and barriers; `saddle plan edit` opens `$EDITOR` and
re-checks; `saddle plan replan --note` asks the planner to revise it;
`saddle plan approve` freezes it at the base commit and `saddle plan reopen`
unfreezes it; `saddle plan push` creates an epic issue and a sub-issue per
task.

Example: `saddle plan gh:#44`.

### `saddle plugin`

Entrypoints for orchestrating from your own Claude Code session:
`saddle plugin setup` (init and doctor on first use), `saddle plugin
install` (checks the binary against the plugin's version), `saddle plugin
brief` (the orchestrator brief), `saddle plugin engine` (the TUI's watchers
without the TUI) and `saddle plugin wait` (blocks until an event needs the
orchestrator).

Example: `/saddle:orchestrate do #46 and #47 in parallel`.
See [README](../README.md#orchestrate-from-claude-code-instead).

### pre-publish gate

Before `prs` or `publish` pushes a layer, Saddle checks out that layer's own
tip in a scratch worktree and runs the lint gate, `[test] cmd` and
`prepublish.cmd` there, bottom to top. A red layer and those above it stay
unpublished. Restack re-runs only `prepublish.cmd`.

Example: `prepublish.cmd = "make check/spelling"`.
See #223.

### `saddle prs`

Pushes landed branches and opens or updates their PRs, laid out as stacks:
dependent or same-topic tasks stack, unrelated ones target base. Each layer
must pass the pre-publish gate first. It publishes the layers below the
first broken, flagged or red one and stops there. It never merges. MCP
tool: `prs`.

Example: `saddle prs`.
See #52.

### `saddle publish`

The escape hatch when `prs` is blocked but one task's work stands on its
own: replays only that task's commits onto base, pushes a fresh branch and
opens an independent PR with `Closes #N`. It refuses work that needs
unmerged tasks below it. Saddle pushes, never the model. MCP tool:
`publish`.

Example: `saddle publish t7 --draft`.
See #220.

### `saddle queue`

Shows the merge train's waiting branches, next first. `saddle queue move
<task> 1` lands a fix next, `saddle queue hold <task>` keeps it from landing
without losing its place ([`on_hold`](#on_hold)), and `saddle queue release
<task>` lets it land again. MCP tools: `queue_move`, `queue_hold`,
`queue_release`.

Example: `saddle queue move t9 1`.
See #146.

### `queued`

A train state: the branch is waiting for `land`.

Example: `t7  done  ...  queued`.

### ref guard

Git hooks (reference-transaction and pre-push) that `saddle init` installs:
only the merge train moves the integration branch, only a task or the train
moves that task's branch, a live task's branch can't be deleted, and only
the train pushes saddle's branches.

Example: a denied `git push` naming the guard.

### `saddle release`

Releases a task's claims, all of them if no glob is given. MCP tool:
`release`.

Example: `saddle release 'internal/foo/**' --task t3`.

### `saddle remote`

An opt-in, read-only MCP endpoint on loopback that lets other Claude Code
sessions read `status` and `needs_you`. `saddle remote serve` runs it;
`saddle remote token create`, `saddle remote token list` and `saddle remote
token revoke` manage the tokens every caller needs.

Example: `saddle remote token create laptop`.
See [docs/remote-control.md](remote-control.md).

### rename map

The renames recorded from every landed commit. The train rebases with it,
applies it to live worktrees, rewrites claim globs and tells affected
agents, so moved directories are followed instead of conflicting.

See [Merge train](ARCHITECTURE.md#merge-train).

### `saddle repair`

Spawns a repair task to re-land a task's conflicting work on a fresh branch
when its agent can't (or is gone). The repair claims the conflicting files,
lands normally, and then supersedes the original and closes its PR.
Restack and land do this by themselves when the owner's window is gone.

Example: `saddle repair t21`.
See #172.

### `repairing`

A train state: a landed task whose commits conflict with base while a
repair task re-lands its work. It is out of the stack meanwhile; restack
drops its commits so the rest of the stack moves on.

See #172.

### `saddle requeue`

Puts a landed task whose work integration lacks back in the merge train,
recreating its branch and worktree if needed. Run `land` afterwards. MCP
tool: `requeue`.

Example: `saddle requeue t21 && saddle land`.
See #119.

### `saddle rescue`

Commits a task's uncommitted work to `rescue/<task>`, writes it as a patch
under `.saddle/rescue/`, then kills the task (claims released, worktree
kept).

Example: `git apply .saddle/rescue/t9.diff`.

### restack

Rebuilds the landed PR stack on `origin/<base>` after base moved, a bottom
PR merged, or the sentinel flagged the stack. Under the train lock it drops
tasks GitHub says are done, replays each stacked task's own commits in train
order (skipping commits base already has), and only when every one applies
moves the task branches and integration, force-with-lease pushes them and
retargets the open PRs. A conflict moves nothing and goes back to the task
that owns the commit, or to a repair task if that task is gone. Restack
never resolves a conflict, never lands queued work and never retargets a
closed or merged PR. Run it through the `restack` MCP tool or `saddle stack
rebase <stack|task|pr>`; auto-merge restacks after each merge.

Example: `stack_at_risk` in status → `restack`.
See [Merge train](ARCHITECTURE.md#merge-train), #119, #190.

### `saddle resume`

Gives an orphaned or paused task a new window in its worktree. A Claude Code
task continues its session (`claude --resume`); any other is relaunched with
its brief.

Example: `saddle resume t6`.
See #254.

### `saddle run`

Runs a command as a [heavy run](#heavy-run): waits for a slot in its class
when the queue enforces, then runs it as if called directly (stdin, stdout,
signals and exit code forwarded). Gives up with exit 75 after `--wait-max`.

Example: `saddle run --class go-test -- make check`.
See [docs/runq.md](runq.md).

### `running`

A task status: the agent is working.

Example: `t3  running  opus  @2  ...`.

### `saddle runq`

Inspects and steers the heavy-run queue: `saddle runq status`, `saddle runq
stats`, `saddle runq slots <class> <n>`, `saddle runq drain [class]` (no new
starts) and `saddle runq kill <lease>`.

Example: `saddle runq slots go-test 2`.
See [docs/runq.md](runq.md).

### scratch root

The disk-backed directory the test gate points `TMPDIR` and `GOTMPDIR` into,
outside the repo (`<user cache dir>/saddle/tmp/<repo hash>` by default, or
`[train] tmpdir`). It is swept of day-old dirs before each run, and a gate
that fails on the environment (disk quota, no space, OOM) is retried, not
blamed on the branch.

Example: `tmpdir = "/var/tmp/saddle-gate"`.
See #184, #250.

### `saddle sentinel`

`saddle sentinel check` runs one [stack sentinel](#stack-sentinel) check now.
`saddle sentinel ack` acknowledges the current at-risk flag and every ci-red
hold when restack and repairs can't fix them: `prs` and `land` stop holding
work back and the `needs-human` labels come off, until a different layer
breaks or the stack checks clean. Auto-merge still never merges a red PR.
MCP tool: `sentinel_ack`.

Example: `saddle sentinel ack`.
See #119.

### serial files

Paths only the merge train may change (`go.sum`, migrations, ...), set by
`serial` in config. Agents ask the orchestrator through `ask_owner` instead
of editing them.

Example: `serial = ["go.sum", "db/migrations/**"]`.

### `saddle spawn`

Starts an agent on its own branch (`saddle/<task>`), worktree and tmux
window, with a prompt and claims. It refuses past the concurrency cap or on
claim overlaps unless forced. Agents spawn sub-tasks the same way through
the `spawn` MCP tool.

Example: `saddle spawn "Glossary" -c 'docs/**' -f prompt.md`.
See [Agents](ARCHITECTURE.md#agents).

### `saddle stack`

Shows the PR stacks as a graph (bases, CI, mergeability, holds, how far
behind base) and manages custom stacks: `saddle stack create`, `saddle
stack add`, `saddle stack remove`, `saddle stack delete`, `saddle stack
list` and `saddle stack show`. `saddle stack rebase` runs
[restack](#restack); `saddle stack link` links stacks on GitHub with
gh-stack; `saddle stack merge` merges a whole stack with `gh stack merge`
and restacks; `saddle stack collapse` retargets a stack's top PR to base and
squash-merges it as one.

Example: `saddle stack --json`.
See #52, #211.

### stack sentinel

The watcher (running under `saddle up` or the plugin engine) that checks
the PR stack for what breaks it: a PR GitHub can't merge, base moving past
integration, branches that drifted. On a hit it raises the
[at-risk flag](#at-risk-flag) and tells the orchestrator to restack. It
never restacks itself. A second watcher handles [ci-red](#ci-red).

See #119.

### `saddle status`

Lists tasks with their status, model, window, claims and train state, plus
warnings, the heavy-run queue while it's busy, and `stack_at_risk` when the
sentinel has flagged the stack. MCP tool: `status`.

Example: `saddle status`.

### `superseded`

A train state: the task's work no longer ships on its own, because it was
killed, its PR was closed, it was unstacked, or a repair re-landed it. Its
commits leave integration on the next restack.

Example: `t13  landed  ...  superseded: 55405c0..6c540d4`.
See #119.

### `saddle sweep`

Merges open `saddle/*` PRs that are green, mergeable, on base (or whose
parent merged), not labelled for review and with tests touched when Go code
is, bottom of each stack first. Off until `[sweeper] enabled = true`;
`--dry-run` only reports.

Example: `saddle sweep --dry-run`.

### `saddle sync`

Rebases the current task's branch onto the integration branch, following
directory moves. The worktree must be clean. After each landing the train
already rebases clean live worktrees unless `[train] no_auto_rebase` is set.

Example: `saddle sync`.

### `t0`

The orchestrator's task id.

Example: `t0  idle  sonnet  ...  orchestrator`.

### task

One unit of work: an id (`t3`), a title, a prompt, a model, a branch
`saddle/<task>`, a worktree under `.saddle/worktrees/`, a tmux window and
claims. Workers can spawn child tasks, which land before their parent.

See [Agents](ARCHITECTURE.md#agents).

### test gate

The train's `[test] cmd`, run on each rebased branch before it is
fast-forwarded in. `land` refuses to run without one.

Example: `cmd = "go test ./..."` under `[test]`.

### `test_failed`

A train state: the branch rebased but the test gate or the lint gate went
red. It goes back to the agent like a conflict.

Example: `t8  conflict  ...  test_failed: tests failed`.

### `saddle trust`

Shows what Saddle does in this repo and asks whether you trust it. The
answer is remembered per repo path and origin URL. `saddle trust status`
shows it.

Example: `saddle trust --yes`.

### `saddle unstack`

Takes landed tasks out of the PR stack for good, by task id, PR URL or PR
number. Their entries become `superseded`, the next restack drops their
commits from integration, and Saddle never touches their PRs again. MCP
tool: `unstack`.

Example: `saddle unstack t13 && saddle stack rebase t14`.
See #12.

### `saddle untrust`

Forgets that this repo is trusted, so `saddle up` and `saddle init` ask
again.

Example: `saddle untrust`.

### `saddle up`

Opens the TUI: the orchestrator chat on the right, agents and a live peek on
the left. While it runs it also hosts the watchers (stack sentinel, ci-red,
CI, auto-merge, autopilot). `ctrl+c` quits and agents keep running.

Example: `saddle up grok epic.md`.
See [README](../README.md#quickstart).

### `saddle version`

Prints the version.

Example: `saddle version`.

### worker

Any task that isn't the orchestrator: an agent writing code in its own
worktree. The orchestrator fetches issues to plan from with `ticket`.

### worktree

The git worktree each task works in, at `.saddle/worktrees/<task>` on branch
`saddle/<task>`. `kill` and `gc` remove it once its work is safe.

See [Layout on disk](ARCHITECTURE.md#layout-on-disk).
