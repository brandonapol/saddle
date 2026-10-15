# Saddle glossary

Every Saddle-specific term, alphabetical. Commands are `saddle <verb>`; most
also exist as an MCP tool of the same name. For the design behind them see
[ARCHITECTURE.md](ARCHITECTURE.md). `saddle <verb> --help` has the flags.

A test (`internal/cli/glossary_test.go`) fails when a command or task state has
no entry here, so add one when you add vocabulary. Keep entries sorted.

### adapter
The CLI an agent runs on: `claude`, `codex`, `gemini` or `grok`. Pick one per
task with `adapter` on spawn; `[adapters] order` is the fallback order when one
runs out of quota. Claude and Grok run saddle's hooks, so their claims are
enforced on every write; Codex and Gemini claims are checked when they
commit and call done. `saddle status` lists which
adapters can run here. See [ARCHITECTURE.md](ARCHITECTURE.md#providers-per-task-choice-and-rotation-321).

### agent
The coding-agent process (Claude Code, Codex, ...) working a task in its own
tmux window and worktree. Also called a worker.

### at-risk
The stack sentinel's flag: a layer of the PR stack is broken (base moved, a
conflict, a drifted branch), so that layer and everything above it is at risk.
`prs` publishes only the layers below it and `land` holds back queued work that
touches the broken layers' files. Fix it with `saddle restack`; acknowledge it
with `saddle sentinel ack`. Status line: `stack_at_risk`.

### automerge
`saddle automerge on|off|status|hold|release|approve`. Off by default. While
`saddle up` runs, merges the bottom PR of a ready stack (green, mergeable, not
a draft, not needs-human, not at-risk), restacks, and repeats. A held stack is
never merged. See [ARCHITECTURE.md](ARCHITECTURE.md#stack-upkeep-when-people-use-github-119-123).

### autopilot
`saddle autopilot on|off|pause|resume|status|infinite`. Off by default. While
on, saddle itself resumes lost agents, lands what is queued, publishes PRs and
tops up to the concurrency cap from open issues labelled `ready` (oldest first,
disjoint `claims:` only), until a stop condition. It never merges; that is
`automerge`. Turning it on asks the orchestrator for a runway plan. A stall
nudge carries saddle's records (owner blockers with their age, tasks, open and
merged PRs, the train) and asks for a lookahead step instead of a status. The
same unchanged idle state is nudged at most three times; then the owner hears
once and the nudges stop until something changes. See also *infinite mode*.

### bench
`saddle bench list|run|score`. The collision bench: works a scenario's dummy
tickets (`bench/scenarios`) with autopilot and fake agents on a copy of
`bench/fixture` in a hermetic world, then scores the run (denied writes, claim
waits, train bounces, forecast hits, tickets that needed help). Run it from a
saddle source checkout. See [bench.md](bench.md).

### blockers
`saddle blockers`: lists open `saddle-blocker` issues, most frequent first.
See [blocker-issues.md](blocker-issues.md).

### brief
`saddle brief [task]`: an agent's goal, owned and hands-off paths, done-when and
children. Also the orchestrator's instructions (`saddle plugin brief`).

### catchup
`saddle catchup`: what happened since you last looked, in at most five bullets.

### changelog
`saddle changelog --version vX.Y.Z [--write]`: release notes from the PRs
merged since the last tag. See [VERSIONING.md](VERSIONING.md).

### checkpoint
A snapshot of a live worker's uncommitted tree, taken every few minutes into
`refs/saddle/checkpoints/<task>` without touching its index or branch. The same
watcher nudges the agent to commit. Pruned when the task lands or is killed;
never pushed.

### ci-red
A stacked PR whose checks failed on its head. Its layer, and every layer above
it, is held (labelled `ci-red`): `prs` doesn't push them, `land` holds related
work, automerge skips them. Saddle spawns a repair; the hold lifts when checks
pass. `saddle sentinel ack` overrides it.

### claim
`saddle claim <glob>...`: reserves paths for a task. A hook denies writes to
files another task has claimed, naming the owner. Unclaimed files are claimed on
first write. `saddle release [glob]...` gives them back. Files in the config's
`serial` list belong to the merge train and can't be claimed.

### completion
`saddle completion <shell>`: shell autocompletion script.

### concurrency
`saddle concurrency [N|reset]`: how many agents may run at once.

### conflict
A task status: its branch came back from the merge train with a conflict (or a
failed gate) and the agent must `saddle sync` and resolve it. As a train entry
state it is the same event. Saddle tries the rename map, a structural merge and
regenerating derived files before it hands the conflict to the agent.

### decisions
`saddle decisions [file|answer|allow|digest]`: the decision queue (#464).
Owner-gated actions (a new dependency, a release tag) wait as decision cards
while other work runs. A reversible card runs its recommended option after its
budget; an irreversible one waits for you. `allow <class> [match]` adds a
`[[preauth]]` rule so a class never waits, and one digest a day
(`[decisions] digest_at`) lists what waits and what saddle decided.

### doctor
`saddle doctor [--fix]`: preflight checks of the repo, `gh`, tools and hooks,
grouped by who fixes each problem.

### done
`saddle done`: an agent marks its task finished. The repo's gate (e.g. `make
check`) must pass, then the task is queued in the merge train. Status `done`.

### down
`saddle down`: stops every agent. Running tasks become `paused`; worktrees and
branches are kept and `saddle up` resumes them.

### escalated
Saddle gave up resolving something itself and told the orchestrator or owner:
a train conflict that failed twice, or a ci-red layer whose repairs ran out.
Shows as `train_escalated` / `escalated` in events and state.

### failed
A task status: its spawn failed. The id stays burned and the reason is in its
summary.

### gc
`saddle gc [--dry-run]`: removes worktrees, branches and refs of killed, failed
or landed tasks, and sweeps the scratch root. Keeps anything with unmerged
work.

### harness
What runs the orchestrator chat and, by default, the workers: `claude`,
`grok` or `codex` (`harness =` in config, or `saddle up grok` for one run). A
task's adapter can differ from the harness.

### heavy run
A CPU-heavy command (`make check`, `go test`, `flutter test`) queued on the
machine-wide heavy-run queue so parallel agents don't saturate the box. See
`saddle run`, `saddle runq` and [runq.md](runq.md).

### hold
Keeps something from moving without losing its place. `saddle queue hold <task>`
keeps a queued branch from landing (`on_hold`); `saddle automerge hold <stack>`
keeps a stack from auto-merging.

### idle
A task status: the agent stopped and waits at its prompt.

### infinite mode
`saddle autopilot infinite on|off` (alt+i in the TUI): autopilot with no stop
condition. An empty ready queue asks the orchestrator to find more work, and a
full plan-limit window *parks* running tasks until it resets.

### init
`saddle init`: creates `.saddle/` with a config template, git excludes and the
ref guard hooks.

### integration branch
`saddle/integration` (`integration` in config). Landed work accumulates here,
one task at a time. It is local until `saddle prs` pushes branches from it.

### kill
`saddle kill <task>`: stops the agent, releases its claims and removes its
worktree. Uncommitted work is snapshotted to `refs/saddle/wip/` first. Status
`killed`.

### killed
A task status: see *kill*. A killed task leaves the PR stack.

### land
`saddle land`: runs the merge train. For each queued branch, one at a time:
rebase onto the integration branch, run the test gate, fast-forward integration.
Run it after agents call `done` (autopilot does it for you). It **never pushes**
and never touches GitHub; publishing is `prs`. A conflict goes back to the
agent that wrote it. Task status becomes `landed`.

### landed
A task status (and train state): its work is on the integration branch. Landed
tasks form the PR stack. See *land*.

### merge train
The serial pipeline that moves finished branches onto the integration branch:
rebase, test, fast-forward, one at a time, so conflicts surface once and go to
the agent that caused them. Driven by `saddle land`. Inspect with `saddle queue`.
See [ARCHITECTURE.md](ARCHITECTURE.md#merge-train).

### merged
A train entry state: base has the task's work, either its PR merged or restack
found it there. It leaves the stack and never comes back.

### message
`saddle message <task> <text>`: sends a `[saddle]` message to a task, waking it
if idle.

### migrate
`saddle migrate`: applies pending `state.db` migrations now, after a backup.

### needs_you
A task status: the agent asked for permission or input. Saddle hands its screen
to the orchestrator, which answers if safe or asks you. Also the top headline
class in `saddle status`. A needs-you card has a stall budget; see
[stall-budget.md](stall-budget.md).

### notices
`saddle notices [--all]`: the orchestrator's pending digest, or every notice and
how it was routed. Action notices wake an idle agent; info notices ride along on
its next tool call.

### on_hold
A train entry state: queued but held by the owner. `land` skips it and it keeps
its place. See *hold*.

### orchestrator
The session that plans, spawns, answers agents and runs the train: a headless
Sonnet in the TUI, or your own Claude Code session through the plugin (task
`t0`). It is not a worker.

### orphaned
Shown, never stored: a live task whose tmux window is gone and whose agent has
been silent for 10 minutes. `saddle resume <task>` gives it a new window.

### park
Stop an agent without losing its work so it frees a slot. Tasks park when their
adapter runs out of quota, when a worker sits on a prompt past the stall budget,
and in infinite mode at the plan limit. Parked tasks are `paused`, keep their
worktree and resume on their own.

### paused
A task status: `saddle down` or a park stopped the agent. It keeps claims and
worktree; `saddle up` or `saddle resume` brings it back.

### peek
`saddle peek <task>`: the last lines of a task's terminal.

### perf
`saddle perf`: times the reads saddle polls and counts piled-up state.

### plan
`saddle plan <epic>`: turns an epic into tasks with disjoint claims, to review,
edit and approve (`approve`, `edit`, `push`, `reopen`, `replan`).

### plugin
`saddle plugin ...`: entrypoints for the Claude Code plugin that lets your own
session be the orchestrator (`brief`, `engine`, `wait`, `setup`, `install`).

### prs
`saddle prs [--dry-run|--push-only|--gate-only]`: after tasks have landed,
checks each layer on its PR's base, pushes the branches and opens or updates
the stack of GitHub PRs in landing order, filling the repo's PR template. This
is where saddle first pushes. See [prs.md](prs.md).

### publish
`saddle publish <task|branch> [--base main] [--draft]`: the escape hatch when
the stack is blocked but one task stands alone. Replays just that task's commits
onto base, pushes them to a fresh branch and opens an independent PR. `prs`
leaves that PR alone. Refuses if the task needs unmerged work below it. See
[ARCHITECTURE.md](ARCHITECTURE.md#stack-upkeep-when-people-use-github-119-123).

### queue
`saddle queue [hold|move|release]`: lists branches waiting to land, next first,
and reorders or holds entries. States: `queued`, `on_hold`.

### queued
A train entry state: `done` and waiting for `land`.

### release
`saddle release [glob]...`: gives back a task's claims (all if none named).
Unrelated to `release` in `queue` / `automerge`, which un-hold.

### remote
`saddle remote ...`: opt-in loopback MCP server so other Claude Code sessions can
see and steer this saddle. See [remote-control.md](remote-control.md).

### repair
`saddle repair <task|pr>`: spawns a task that re-lands conflicting work on a
fresh branch when the owner's agent can't. Restack, land and ci-red do this
automatically; this triggers it by hand.

### requeue
`saddle requeue <task>`: puts a landed task whose work the integration branch
lacks back in the merge train, recreating its branch if needed; then run
`saddle land`. Never edit `state.db` instead.

### rescue
`saddle rescue <task>`: commits a task's uncommitted work to `rescue/<task>`,
writes a patch under `.saddle/rescue/`, and kills the task, keeping its worktree.

### restack
`saddle restack [--continue|--abort]`: rebuilds the landed stack on origin's
base after base moved, a bottom PR merged, or the sentinel flagged the stack.
Rebases in train order, moves branches, pushes and retargets PRs. A conflict
moves nothing and goes back to the task that owns the commit. It never uses
git by hand or a worker. `--continue` / `--abort` recover an interrupted run
([restack-recovery.md](restack-recovery.md)).

### resume
`saddle resume <task>`: opens a new window for a paused or orphaned task,
continuing its agent session. `saddle up` does this for every windowless task.

### run
`saddle run --class <class> -- <cmd>`: runs a heavy command when the heavy-run
queue gives it a slot, forwarding stdin, signals and the exit code. See
*heavy run*.

### running
A task status: the agent is working.

### runq
`saddle runq status|drain|kill|slots`: inspects and steers the heavy-run queue.
See [runq.md](runq.md).

### scratch root
The one directory for saddle's temp files (`[tmp] dir`, default
`~/.cache/saddle/tmp`), exported as `TMPDIR`, swept automatically and by `saddle
gc`. Agents use `$TMPDIR`, never a hardcoded `/tmp` path. See [scratch.md](scratch.md).

### sentinel
The stack sentinel: a watcher (in `saddle up` or the plugin engine) that checks
every layer of the PR stack and sets or clears the *at-risk* flag. It also runs
the *ci-red* watcher. `saddle sentinel check` runs one cycle; `saddle sentinel
ack` acknowledges the flag and ci-red holds so they stop holding work back until
a different layer breaks.

### serial file
A file only the merge train may change (lockfiles, migrations), listed in
`serial` in config. Agents can't claim them; they ask the orchestrator.

### spawn
`saddle spawn`: starts an agent on its own branch, worktree and tmux window,
with a brief and claims.

### sre
`saddle sre check-config`: validates `.saddle/sre.toml`, the per-repo SRE
mode config of known checks, endpoints and credential references (see
`docs/sre.md`). Plaintext secrets are rejected.
During an incident, the read-only production guard holds investigators
read-only until a root cause is stated (`saddle sre root-cause`) and verified
by a different agent (`saddle sre verify`); even then a production action is
filed in the decision queue as irreversible, not run. `saddle sre incident`
shows the state; `saddle sre close` lifts the guard.

### stack
The landed tasks that have open PRs, chained so each PR's base is the one below.
`saddle stack` draws them with CI, mergeability, holds and distance behind base.
`saddle stack create|add|remove|delete|list|show|link|merge|collapse|rebase`
manage owner-defined stacks. Merged, closed and killed tasks leave it by themselves.

### stall budget
How long a needs-you decision card waits for an answer (default 30m) before
saddle takes its non-destructive route-around default. See [stall-budget.md](stall-budget.md).

### status
`saddle status`: tasks, claims, the train, and heavy runs. A task status is one of
`running`, `idle`, `needs_you`, `done`, `conflict`, `landed`, `killed`, `paused`,
`failed`, or the shown-only `orphaned`.

### superseded
A train entry state: a closed PR, a killed task or `saddle unstack` took the task
out of the stack for good. Restack drops its commits from integration.

### sweep
`saddle sweep [--dry-run]`: merges open `saddle/*` PRs that are green, mergeable
and tested, bottom first. Off until `[sweeper] enabled = true`.

### sync
`saddle sync`: rebases this task's branch onto the integration branch, following
directory moves. Agents run it after something lands.

### task
The unit of work: an id (`t7`), a branch `saddle/<id>-<slug>`, a worktree, claims
and a status. The orchestrator is `t0`.

### test_failed
A train entry state: the test gate failed after the rebase, so the branch went
back to its agent.

### trust
`saddle trust` / `saddle untrust`: mark this repo as trusted (or forget it) so
saddle may set up and run agents here.

### unstack
`saddle unstack <task|pr>...`: takes landed tasks out of the PR stack for good.
Their entries become `superseded`, the next restack drops their commits, and
saddle never touches their PRs again.

### untrust
See *trust*.

### up
`saddle up [claude|grok|codex] [epic]`: opens the TUI (chat with the
orchestrator, agent list, live peek) and hosts the watchers. Agents keep
running when it exits.

### version
`saddle version [--long]`: prints the saddle version.

### worktree
Each task's private git checkout under `.saddle/worktrees/`, on its own branch.
Removed on `kill`, kept on `down`.
