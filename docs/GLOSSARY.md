# Saddle glossary

Every Saddle-specific term, alphabetical. Commands are `saddle <verb>`; most
also exist as an MCP tool of the same name. For the design behind them see
[ARCHITECTURE.md](ARCHITECTURE.md). `saddle <verb> --help` has the flags.

A test (`internal/cli/glossary_test.go`) fails when a command or task state has
no entry here, so add one when you add vocabulary. Keep entries sorted.

## How to read and extend this

- One word, one meaning. An entry may end with *Avoid:* (words not to use
  for this idea, in code, docs, notices or tickets) and *Maps to:* (the
  status, train state, table, event or file that stores it).
- When a word you need has a second meaning in the code, don't pick one
  silently: add the meaning here, and file a rename issue for the other use.
  [Open renames](#open-renames) lists the ones not done yet.
- Decisions behind these words are in [docs/adr/](adr/README.md). Ideas that
  were considered and rejected are in [.out-of-scope/](../.out-of-scope/README.md).

## Settled words

These are the words the PR-graph engine (#779) and every new ticket build on.
Each has a full entry below.

| Word | Means | Never means |
|---|---|---|
| *landed* | the train put the task's work on the integration branch; its PR is open or opening | merged, done, on main |
| *merged* | base has the task's work; the only done state | landed, PR opened |
| *held* | not a word on its own: always say which hold (see *held*) | — |
| *stack* | one chain of PRs, each based on the PR of a task it depends on | every landed task; the integration branch |
| *layer* | one task's place in a stack: its commit range, branch and PR | a train entry; a position in train order |
| *parked* | an agent saddle stopped to free a slot; it resumes itself | an owner pause; a red PR set aside |
| *repair* | a new task saddle spawns to fix another task's landed work | a sync, a rescue, the owner's agent fixing its own conflict |

### adapter
The CLI an agent runs on: `claude`, `codex`, `gemini` or `grok`. Pick one per
task with `adapter` on spawn; `[adapters] order` is the fallback order when one
runs out of quota. Claude and Grok run saddle's hooks, so their claims are
enforced on every write; Codex and Gemini claims are checked when they
commit and call done. `saddle status` lists which
adapters can run here. See [ARCHITECTURE.md](ARCHITECTURE.md#providers-per-task-choice-and-rotation-321).

### after
An edge saying a task builds on another task's work, so prs stacks its PR on
that task's even when their files don't overlap. Set at spawn (`after`) or
later with `saddle after <task> <dep>...` (MCP tool `after`); both write
`.saddle/deps/<task>`. When a layer fails the pre-publish gate on something a
sibling adds, prs names the probable edge and the command, but never records
it by itself.

### agent
The coding-agent process (Claude Code, Codex, ...) working a task in its own
tmux window and worktree. Also called a worker.

### at-risk
The stack sentinel's flag: a layer of the PR stack is broken (base moved, a
conflict, a drifted branch), so that layer and everything above it is at risk.
`prs` publishes only the layers below it and `land` holds back queued work that
touches the broken layers' files. Fix it with `saddle restack`; acknowledge it
with `saddle sentinel ack`. Status line: `stack_at_risk`.
*Avoid:* "at risk" for red CI (that is *ci-red*), "blocked", "broken stack".
*Maps to:* `.saddle/stack-at-risk.json` (`App.Flag`), events `stack_at_risk` /
`stack_clear`. A flag *covers* the flagged layer and the layers above it in
its own stack only (#771), not later tasks in train order. `atRisk()` in
`internal/app/automerge.go` also counts ci-red; renaming it is #862.

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

### change
The PR-graph engine's unit (proposed in [ADR 0009](adr/0009-prgraph-engine.md),
#779): one task's work with a fixed branch, its current head, its target base,
its dependency edges and, once published, its PR. Created when the task calls
`done`; a repair or rebase gives it a new head, never a new change. A change is
a *layer* while it has a PR in a stack. Its states are `queued`, `landed`,
`published`, `green`, `red` (with a cause), then `merged` or `superseded` (with
a reason: `closed`, `killed`, `unstacked`, `gave_up`). Every non-terminal state
has a deadline and an automatic exit; a retry time is a field, never a state.
*Avoid:* "parked" or "dropped" for a change state (use `red` with a retry time,
and `superseded`); "entry" (a train row).
*Maps to:* `prgraph_change`, `prgraph.Change` (`internal/prgraph`, #829).

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
*Avoid:* "failed", "broken", "at risk" for this.
*Maps to:* `.saddle/ci-red.json` (`App.CIRed`); the layers it holds come
from `ciRedHolds` (`internal/app/cired.go`). The ci-red hold is one kind of
*held*.

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

### decision card
One needs-you ask with numbered options, a recommended default and a stall
budget. Kinds (`app.DecisionKinds`): `stuck-stack` (a layer red or conflicting
with nobody fixing it), `ci-repair-cap` (CI repairs ran out), `ask-owner` (an
agent's question), `risky-prompt` (a permission prompt saddle won't answer
alone), `owner` (filed through `saddle decisions`: a new dependency, a release
tag), `no-progress` (an autopilot stall), `done-orphaned` (a task called done
but its branch never queued), `worker-hung` (an agent stopped answering) and
`prompt-wait` (an agent waiting at a prompt). A card closes by the owner's
answer, its timeout default, a `[[preauth]]` rule, or the situation resolving.
*Avoid:* "ticket", "alert", "notice" (a notice is a message, not a choice).
*Maps to:* `.saddle/decisions.json`, events `decision_*`.

### decisions
`saddle decisions [file|answer|allow|digest]`: the decision queue (#464).
Owner-gated actions (a new dependency, a release tag) wait as decision cards
while other work runs. A reversible card runs its recommended option after its
budget; an irreversible one waits for you. `allow <class> [match]` adds a
`[[preauth]]` rule so a class never waits, and one digest a day
(`[decisions] digest_at`) lists what waits and what saddle decided.

### delivery
How far a landed task's work has got toward base, in the words status, the
TUI, notices and the orchestrator use: `landed locally, no PR yet`, `landed
locally, PR open`, then `merged`. Only `merged` is done. Shown as `delivery`
in the status tool and in place of `landed` in `saddle status`. See
[ADR 0003](adr/0003-landed-means-on-main.md).

### doctor
`saddle doctor [--fix]`: preflight checks of the repo, `gh`, tools and hooks,
grouped by who fixes each problem.

### done
`saddle done`: an agent marks its task finished. The repo's gate (e.g. `make
check`) must pass, then the task is queued in the merge train. Status `done`
means queued, not delivered: the work counts as done for the owner only once
its PR has merged into base (see *delivery*).

### down
`saddle down`: stops every agent. Running tasks become `paused`; worktrees and
branches are kept and `saddle up` resumes them.

### escalated
Saddle gave up resolving something itself and told the orchestrator or owner:
a train conflict that failed twice, or a ci-red layer whose repairs ran out.
Shows as `train_escalated` / `escalated` in events and state.
*Avoid:* "stuck", "failed", "parked" for this.
*Maps to:* train state `escalated` (`app.TrainEscalated`), events
`train_escalated`, `ci_red_escalated`; a `saddle-blocker` issue when it
repeats.

### failed
A task status: its spawn failed. The id stays burned and the reason is in its
summary.

### folded
A train entry state: a CI repair whose commits joined the red PR it repaired.
It has no PR of its own and is out of the landed list; its work ships in that
PR. *Avoid:* "merged", "superseded", "squashed".
*Maps to:* `train.state = 'folded'` (`app.TrainFolded`), event
`ci_repair_folded`.

### gate
A check that must pass before work moves on. Kinds:
- **lint gate**: the repo's own pre-commit/lint check (`[train] lint.cmd`,
  detected when unset). `done` runs it in the worktree.
- **test gate** (land gate): `[test] cmd` and the lint gate, run by the train
  after the rebase, plus the secret scan. Red is `test_failed`.
- **pre-publish gate** (#223): before `prs` pushes a layer, each layer's own tip
  is checked on its PR's base, bottom to top (`[train] prepublish.cmd`). A red
  layer and those above it stay unpublished; a gate-red repair may follow.
- **write gate**: the conditions in `internal/gate` behind a *write hold*.
*Avoid:* "CI" for any of these (CI is the forge's checks on a pushed PR, see
*ci-red*); "the gate" without the kind when more than one could apply.

### gc
`saddle gc [--dry-run]`: removes worktrees, branches and refs of killed, failed
or landed tasks, and sweeps the scratch root. Keeps anything with unmerged
work. *Automatic gc* is the same run on a 30-minute timer in the engine and
`saddle up` (the `gc` watcher, #802): it skips while the train is busy, only
removes things of tasks the store knows are dead, keeps the newest
integration backups, packs loose objects and prunes old noisy event rows
(`gc_auto` events).

### harness
What runs the orchestrator chat and, by default, the workers: `claude`,
`grok` or `codex` (`harness =` in config, or `saddle up grok` for one run). A
task's adapter can differ from the harness.

### heavy run
A CPU-heavy command (`make check`, `go test`, `flutter test`) queued on the
machine-wide heavy-run queue so parallel agents don't saturate the box. See
`saddle run`, `saddle runq` and [runq.md](runq.md).

### held
Not a word on its own. Nine different mechanisms keep work from moving, and
#771, #774 and #776 were bugs where one subsystem read another's "held". Always
name the kind:

| Kind | What is held, and until when | Who places it | Maps to |
|---|---|---|---|
| queue hold | a queued branch doesn't land; keeps its place until released | owner (`saddle queue hold`) | train state `on_hold` |
| automerge hold | a stack is never auto-merged until released | owner (`saddle automerge hold`) | event `automerge_hold`, automerge state |
| land hold | `land` skips a queued branch that touches files the at-risk or ci-red part of the stack changed; it stays `queued` and lands once those layers clear | saddle (`land`) | land result `held` (`app.TrainHeld`), `frozenFiles` / `ciRedFrozen` in `internal/app/train.go` |
| ci-red hold | the layers above a red layer: not pushed, not auto-merged, until its checks pass | saddle (CI watcher) | `.saddle/ci-red.json`, `ciRedHolds` |
| at-risk cover | the flagged layer and the layers above it in its own stack: not auto-merged | saddle (sentinel) | `flagCovers`, `.saddle/stack-at-risk.json` |
| dependency hold | a layer stays on its current base while a task it runs `after` has an open PR outside its stack | saddle (`prs`, restack) | event `dependency_hold` (`waitingOn`, `internal/app/prbase.go`) |
| route-around hold | the layers built on a red layer that was routed around wait off the stack until it merges, or are requeued without its work once its PR closes or its task is killed or ends with no change; a route still unresolved after 24h is dropped with a blocker issue | saddle (stall budget) | `.saddle/stall-held.json` (`StallHeld`) |
| write hold | a task may not write some paths until a condition (another task lands, a PR merges, a time) | saddle (spawn, scheduler) | `internal/gate` `Hold`; denied writes say `held (<id>)` |
| reconcile hold | the PR-graph engine plans but applies no action, until it expires (24h at most) | owner (`saddle reconcile hold --for`, proposed in [ADR 0009](adr/0009-prgraph-engine.md)) | planned, with the first cutover (#832); the #830 shadow never acts, so has nothing to hold |

Every hold that saddle places must lift on its own or offer a self-service
escape (#119): a dependency hold drops when the dependency leaves for good
(`dep_dropped`, #774); a route-around must not strand its layers (#776).
*Avoid:* bare "held", "blocked", "frozen", "waiting" without the kind. Renaming
the clashing code names (`TrainHeld`, `stall-held.json`) is #862.

### hold
The verb: keep something from moving without losing its place. The owner's
two holds are `saddle queue hold <task>` (a queue hold, train state
`on_hold`) and `saddle automerge hold <stack>` (an automerge hold). Saddle
places the other kinds itself; all of them are listed under *held*.

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
`saddle/integration` (`integration` in config): the merge train's internal,
local base. Each queued branch is rebased and tested on top of the work landed
before it here, and the stack's landed ranges are recorded on it. It is not a
destination: work on it is "landed locally", not done, and landing opens the
task's PR against base right away. See
[ADR 0003](adr/0003-landed-means-on-main.md).

### invariant violation
A state that breaks one of [ADR 0004](adr/0004-reconcile-against-github.md)'s
invariants, such as a landed task with no PR (`done_not_on_main`) or a repair
of `saddle/base` (`self_repair`). The invariant monitor checks for them every
sentinel tick (#825) and reports each one once, as an event and a notice to the
orchestrator, until it clears; open ones show in `saddle status` and
`saddle doctor`. It only detects; the reconciler heals. *Maps to:* events
`invariant_violated` and `invariant_cleared`, `.saddle/invariants.json`.

### kill
`saddle kill <task>`: stops the agent, releases its claims and removes its
worktree. Uncommitted work is snapshotted to `refs/saddle/wip/` first. Status
`killed`.

### killed
A task status: see *kill*. A killed task leaves the PR stack.

### land
`saddle land`: runs the merge train. For each queued branch, one at a time:
rebase onto the work landed before it, run the test gate, fast-forward the
train's local base. Then, in the same step, it opens the PR of every landed
task that has none against base (stacked on another task's PR only when it
depends on it), the way `prs` does; with auto-merge on, that PR is carried to
base. Nobody needs to remember `prs`. Run land after agents call `done`
(autopilot does it for you). A conflict goes back to the agent that wrote it.
Task status becomes `landed`: landed locally, not done until its PR merges
(see *delivery*). A repo with no remote lands locally only.

### landed
A task status (and train state): the train landed its work locally, on the
integration branch, and its PR is open or opening. Landed is **not done**:
only *merged* is. A landed task with no PR is an alarm (`landed_no_pr` event, a
notice, `saddle doctor`'s "landed without a PR") that saddle heals by opening
the PR on the next land or sentinel cycle. See *land*, *delivery* and
[ADR 0003](adr/0003-landed-means-on-main.md).
*Avoid:* "merged", "shipped", "done", "in main", "delivered" for a landed task.
When talking to the owner, say "landed locally, PR open".
*Maps to:* `tasks.status = 'landed'`, `train.state = 'landed'`
(`store.TrainOK`).

### landed list
Every task in the landed state, in train order: the rows the train landed and
that haven't merged or left. It holds many stacks, not one. Train order says
nothing about which layer builds on which; dependencies do (#771).
*Avoid:* "the stack" (the code's `landedStack()`; rename is #863).
*Maps to:* `train` rows with state `landed`, plus their ranges on the
integration branch.

### layer
One landed task's place in a stack: its commit range on the integration
branch (`From..To`), its branch and its PR. The layers *above* a layer are
the ones whose PRs are based, directly or through others, on its PR. Layers
in different stacks are never above or below each other, whatever their train
order.
*Avoid:* "entry" (a train row), "level", "PR" when you mean the commit range,
"position".
*Maps to:* `landedTask` (`internal/app/train.go`).

### merge train
The serial pipeline that lands finished branches and opens their PRs:
rebase onto the work landed before, test, fast-forward, one at a time, so conflicts surface once and go to
the agent that caused them. Driven by `saddle land`. Inspect with `saddle queue`.
See [ARCHITECTURE.md](ARCHITECTURE.md#merge-train).

### merged
A train entry state: base has the task's work, either its PR merged or restack
found it there. It leaves its stack and never comes back. This is the only
state in which a task's work counts as done.
*Avoid:* "landed" for this; "closed" (a closed PR is *superseded*, not merged).
*Maps to:* `train.state = 'merged'` (`app.TrainMerged`), delivery `merged`.

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
*Avoid:* "held" alone (that is any of eight things); say "queue hold".

### orchestrator
The session that plans, spawns, answers agents and runs the train: a headless
Sonnet in the TUI, or your own Claude Code session through the plugin (task
`t0`). It is not a worker.

### orphaned
Shown, never stored: a live task whose tmux window is gone and whose agent has
been silent for 10 minutes. `saddle resume <task>` gives it a new window.

### outage
The PR-graph engine's word (proposed in [ADR 0009](adr/0009-prgraph-engine.md))
for one cause turning many changes red at once: CI not reporting, a gate red
on base's own tip, a forge refusal. It gets one escalation and no repairs, and
the affected changes' clocks pause until it clears. Base-red is one kind.
*Avoid:* "incident" (a GitHub issue label), "ci-red" for this.
*Maps to:* planned, #834.

### park
Stop an agent without losing its work so it frees a slot. Tasks park when their
adapter runs out of quota, when a worker sits on a prompt past the stall budget,
and in infinite mode at the plan limit. A parked task is `paused`, keeps its
claims and worktree, and saddle resumes it by itself when the reason clears.
*Avoid:* "parked" for a red PR labelled `needs-human` and dropped from the ci-red
hold (call that *set aside*; the `stall_parked` event still says parked, #862);
"paused" when you mean saddle did it; "sleeping", "suspended".
*Maps to:* status `paused` plus a park event (`stall_parked`, handoff
`park`); the difference from an owner pause is who resumes it.

### paused
A task status: `saddle down` or a park stopped the agent. It keeps claims and
worktree; `saddle up` or `saddle resume` brings it back.
*Avoid:* "parked" when the owner stopped it: parked means saddle did and
will resume it; paused by `saddle down` waits for `saddle up`.

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

### PR layout
How `prs` turns the landed list into PRs: for each layer, which PR it is
based on and what its branch holds, found by replaying the layer's commits
onto that base in a scratch worktree (`openLayout`, `.saddle/layout-*`).
*Avoid:* "restack" (that moves already-published layers after base moved).

### prs
`saddle prs [--dry-run|--push-only|--gate-only]`: checks each landed layer on
its PR's base, pushes the branches and opens or updates the stack of PRs in
landing order, filling the repo's PR template. Land and the sentinel run the
same publish themselves, so prs is for refreshing the stack, previewing it,
and seeing why a PR didn't open. See [prs.md](prs.md).

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

### reconcile
`saddle reconcile [--plan] [--replay <state.db> --day YYYY-MM-DD]`: the
PR-graph engine (`internal/prgraph`, [ADR 0009](adr/0009-prgraph-engine.md))
in *shadow*. `--plan` (the default) prints what it would do for the stack now;
`--replay` replays a copy of a state.db's events against it offline and lists
the divergences and outcome counts. Neither acts. See *shadow*.

### release
`saddle release [glob]...`: gives back a task's claims (all if none named).
Unrelated to `release` in `queue` / `automerge`, which un-hold.

### remote
`saddle remote ...`: opt-in loopback MCP server so other Claude Code sessions can
see and steer this saddle. See [remote-control.md](remote-control.md).

### repair
A new task saddle spawns to fix another task's landed work, because the task
that wrote it can't (its window is gone, or it already failed). Kinds:
- **conflict repair** (#172): restack or land hit a conflict; the repair re-lands
  the original's work on a fresh branch. The original is *repairing* until the
  repair lands, then *superseded*.
- **CI repair** (#196): a layer is *ci-red*; the repair's commits go onto the
  red PR's branch and its train entry is *folded*. Capped by
  `[ci] repair_attempts`, then a `ci-repair-cap` decision card.
- **gate-red repair** (#773): a layer is red at the *pre-publish gate*.

`saddle repair <task|pr>` triggers a conflict repair by hand. A repair of a
repair is a sign the base it was cut from moved under it (#779).
*Avoid:* "fix task", "fix-up", "rescue" (that is `saddle rescue`), "sync" (an
agent rebasing its own branch).
*Maps to:* events `repair_needed` / `repair_spawned` / `repair_failed`,
`ci_repair_*`; train states `repairing`, `folded`.

### repairing
A train entry state: a landed task whose commits conflict with base while a
conflict *repair* re-lands its work. It is out of its stack; restack drops
its commits and saddle leaves its PR alone until the repair lands and the
task becomes *superseded*.
*Avoid:* "held", "parked", "escalated".
*Maps to:* `train.state = 'repairing'` (`app.TrainRepairing`).

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

### route-around
What the stall budget does when a red layer has blocked its stack too long:
the red layer leaves the stack (keeping its own PR), and the layers built on
it wait off the stack with it (a route-around hold) until it merges; then
they go back on top. A truncated route-around (#766) has no PR because the
red layer couldn't stand alone. When the red layer will never merge (its PR
is closed, or its task is killed or ends with no change) its layers are
requeued to rebuild on base without its work; a route unresolved after 24h
is dropped with one blocker issue and a `saddle requeue` hint (#776).
*Maps to:* `.saddle/stall-held.json` (`StallRoute`; rename in #862).

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

### set aside
What the stall budget does to a red layer whose CI repairs ran out and whose
card got no answer: its PR is labelled `needs-human` and its ci-red hold is
acknowledged, so it stops holding the layers above it. The agent is not
stopped. *Avoid:* "parked" (the current `stall_parked` event; #862).

### shadow
The PR-graph engine running beside saddle's own code without acting (#830,
ADR 0009 §6): a watcher (`reconcile-shadow`, in `saddle up` and the plugin
engine) builds prgraph's input from today's stores every sentinel tick and
logs `reconcile_shadow` events: `prgraph_only <action>` for what prgraph would
do and saddle didn't within 10 minutes, `legacy_only <action>` for what saddle
did and prgraph wouldn't. Ticks whose input changed are kept in
`prgraph_tick` for 14 days, for replay. A cutover to prgraph needs a clean
shadow first. *Avoid:* "dry run" (that is `--dry-run` on one command).

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

### sso
`saddle sso login|status|logout|token`: one OIDC sign-in (Keycloak, Okta)
whose tokens feed codex agents through codex's command-backed provider auth.
`saddle up` signs in when needed; a lapsed login sends its sign-in URL to
the orchestrator. See [sso.md](sso.md).

### stack
One chain of PRs: each PR's base is the PR of a task it depends on (`after`,
a spawn parent, the same issue, or what the pre-publish gate needs), and the
bottom PR's base is base. A landed task with no dependencies is a stack of
one, with its PR on base (#751). Sharing a file does not put two tasks in one
stack. *Custom stacks* are owner-defined (`saddle stack
create|add|remove|delete|list|show|link|merge|collapse|rebase`); a *native
stack* is the same chain linked on GitHub with gh-stack. `saddle stack` draws
them with CI, mergeability, holds and distance behind base. Merged, closed and
killed tasks leave by themselves.
*Avoid:* "the stack" for every landed task in train order (that is the
*landed list*; the code still calls it `landedStack`, #863); "stack" for the
integration branch; "queue" (that is the train).
*Maps to:* PR bases saved in `.saddle/pr-bases.json`, edges in
`.saddle/deps/<task>`, owner stacks in `.saddle/stacks.json`.

### stall budget
How long a needs-you decision card waits for an answer (default 30m) before
saddle takes its non-destructive route-around default. See [stall-budget.md](stall-budget.md).

### status
`saddle status`: tasks, claims, the train, and heavy runs. A task status is one of
`running`, `idle`, `needs_you`, `done`, `conflict`, `landed`, `killed`, `paused`,
`failed`, or the shown-only `orphaned`.

### superseded
A train entry state: a closed PR, a killed task, a repair that replaced it, or
`saddle unstack` took the task out of its stack for good; its work no longer
ships on its own. Restack drops its commits from integration.
*Avoid:* "merged", "folded" (its work joined another PR), "dropped".
*Maps to:* `train.state = 'superseded'` (`app.TrainSuperseded`).

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

## Open renames

Code names that still clash with this glossary. Drop a row when its issue
lands.

| Word | Clash | Issue |
|---|---|---|
| held | land result `held` (`TrainHeld`), `stall-held.json`, `restack_held` | #862 |
| parked | `stall_parked` for a red PR set aside | #862 |
| at-risk | `atRisk()` also counts ci-red | #862 |
| stack | `landedStack()` and "the stack" for the landed list | #863 |
| landed | the integration branch itself goes away | #836 |
