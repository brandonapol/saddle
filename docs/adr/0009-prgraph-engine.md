# ADR 0009: One pure PR-graph engine owns the stack: one schema, one state machine, one Reconcile

- Status: Proposed
- Issue: #779 (step 1, the ADR). Implementation: #829, #830, #831, then
  #832, #833, #834, #835, #836. Related: ADR 0003, ADR 0004 (#820), ADR 0005,
  ADR 0007, ADR 0008, #804, #862, #863

## Context

ADR 0004 decided *that* one reconciler replaces saddle's per-feature PR
logic, and listed the invariants I1-I11. It left #779's own questions open,
and #829 can't start without them: what the engine stores, which states a
change can be in and how each one ends, what exactly `Reconcile` promises,
and where the package boundary runs so it can later move to `~/code/sddl`.

The evidence is in #779 and ADR 0004 and isn't repeated here. In short:
31% of quark's tasks were repairs (29 repairs of repairs) because repairs
are cut from `saddle/integration`; stack state is split over about 12 JSON
files plus the `train` table with unlocked read-modify-writes (`SetCIRed`,
13 call sites); and every subsystem has its own "held" and its own escape
hatch.

Two inputs from the epic conflict with settled words and are resolved
here, not silently: #779 names layer states `parked` and `dropped`, but
the glossary reserves *parked* for an agent saddle stopped, and lists
"dropped" under *superseded*'s *Avoid*. And #779 counts "file overlap with
its commits" as a dependency, which ADR 0007 rejects.

## Decision

### 1. The unit is a *change*; state lives in one SQLite schema

A **change** is one task's work as the engine sees it: a fixed branch, its
current head, its target base, its dependency edges and, once published,
its PR. It is created when the task calls `done` and keeps its id and
branch for life. A repair, a conflict fix or a rebase gives the change a
**new head**; it never creates a new change. So "repair of a repair" can't
happen by construction: there is nothing for the second repair to inherit.
A change is a *layer* from the moment it has a PR until it leaves its
stack. The id is opaque to the engine; saddle uses the task id.

prgraph owns these tables, with its own migration version, in whatever
`*sql.DB` the host passes. Saddle passes `.saddle/state.db` (ADR 0005).
Every table is prefixed `prgraph_` so sddl can later use its own file.

```sql
prgraph_change(
  id TEXT PRIMARY KEY, branch TEXT NOT NULL UNIQUE, base TEXT NOT NULL,
  head TEXT NOT NULL,              -- the work the host submitted (intent)
  seq INTEGER NOT NULL,            -- submission order: tie-break only, never a dependency
  pr INTEGER,                      -- cache; the forge snapshot wins
  state TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '',
  since INTEGER NOT NULL, deadline INTEGER, retry_at INTEGER,
  automerge INTEGER NOT NULL DEFAULT 1, hold_until INTEGER,
  gen INTEGER NOT NULL)            -- CAS version for every write to the row
prgraph_edge(change TEXT, dep TEXT,
  kind TEXT CHECK(kind IN ('declared','inferred','verified')),
  source TEXT NOT NULL,            -- after | parent | issue | owner_stack | hand_retarget | gate
  PRIMARY KEY(change, dep))
prgraph_check(head TEXT, name TEXT, status TEXT, required INTEGER,
  host_owned INTEGER, observed_at INTEGER, PRIMARY KEY(head, name))   -- CI facts, a cache
prgraph_gate(head TEXT, onto TEXT, status TEXT, detail TEXT, ran_at INTEGER,
  PRIMARY KEY(head, onto))         -- gate facts: onto = base sha + dep heads; immutable
prgraph_attempt(key TEXT PRIMARY KEY, kind TEXT, inputs TEXT, n INTEGER,
  next_at INTEGER, first_err TEXT, last_err TEXT, escalated INTEGER)  -- #826
prgraph_tick(id INTEGER PRIMARY KEY, ts INTEGER, input_hash TEXT,
  input BLOB, plan BLOB)           -- recorded inputs and plans, for replay; pruned after 14 days
```

Stored rows are **intent** (branch, head, edges, policy), **facts that
cost work to learn** (gate results per head and base, CI results per
head) and **history** (attempts, ticks). Everything else (PR base, PR
state, mergeability, "is it in base") is read from the forge and git every
tick, and a stored copy is only a cache that the next tick overwrites (I2).
"Merged" is decided by git: base contains the change's patch-ids or its
`Saddle-Task` trailer, whatever the rows say.

What replaces the stack's files:

| Today | After |
|---|---|
| `deps/<task>` | `prgraph_edge` |
| `pr-bases.json`, `published.json` | `prgraph_change.pr` plus the snapshot; the desired base is computed from edges |
| `stacks.json` (owner stacks) | `declared` edges with `source = owner_stack`; collapse is a view |
| `prgate.json` | `prgraph_gate` (also the pass stamp #800 wants) |
| `ci-red.json`, `ci-seen.json` | `prgraph_check` plus state `red` |
| `automerge.json` | `prgraph_change.automerge` and `hold_until`; the `Stopped` state is gone (#826) |
| `stack-at-risk.json`, `stuck-stack.json`, `stall-held.json` | nothing: holds are computed every tick |
| `restack-journal.json` | nothing: actions are CAS-guarded, no journal (#777) |
| `train` rows past `queued` (`landed`, `merged`, `superseded`, `folded`, `repairing`) | `prgraph_change.state`; `folded` and `repairing` disappear because a repair is a new head |
| `decisions.json` | stays saddle's; only the stuck-stack and ci-repair-cap producers go |

The `train` table keeps only the queue (`queued`, `on_hold`, order) until
#836 moves that into `prgraph_change` too. `autopilot.json`,
`concurrency.json`, `watchdog.json`, `up.json` and `blocker-issues.json`
are not about the PR graph and stay.

### 2. The change state machine: every non-terminal state has a deadline and an exit

| State | Means | Deadline (default) | Automatic exit |
|---|---|---|---|
| `queued` | `done`; this head hasn't passed the train's gate on base + its deps | 2h of gate attempts | gate green → `landed`; gate red or conflict → `red` |
| `landed` | gate green on base + deps; PR not open on its desired base yet | 30m | `Publish`/`Move` → `published`; deadline → `red(publish)` |
| `published` | PR open on its desired base; required checks pending | 3h | checks green → `green`; red → `red`; deadline → `red(checks_stuck)`, which re-runs the checks |
| `green` | required, non-host checks green at the head on its desired base | 1h once its deps are merged | `Merge` → `merged`; refused or failed → retried through #826; deadline → `red(merge)`; with automerge off, deadline → a line in the owner's daily digest and a new deadline |
| `red(cause)` | a required non-host check, the gate, a rebase or a forge rule is red for this change alone | `retry_at` per fix attempt; cap `[ci] repair_attempts` | new head → `queued`; cap reached → `superseded(gave_up)` with one blocker issue |
| `merged` | base contains the work | terminal | — |
| `superseded(reason)` | leaves for good: `closed`, `killed`, `unstacked`, `gave_up` | terminal | — |

Rules a reviewer checks the code against:
- **A clock runs only while the change itself could move.** Waiting on an
  unmerged dependency, on a red base (§3), or on a forge that answers
  `ErrTransient`/`ErrRateLimited` pauses the deadline. Those waits are
  bounded anyway: the dependency has its own deadlines, base-red has its
  own repair, and the forge waits are I11's. So the whole graph reaches
  terminal states within a bound computed from the DAG's depth.
- **`retry_at` is a field, not a state.** #779's `parked(reason, retry_at)`
  is `red` (or any state) with `retry_at` set. The word *parked* stays the
  agent's (glossary).
- **#779's `dropped` is `superseded`.** "Human-only" is the `needs-human`
  label and blocker issue on a `superseded(gave_up)` change, never a frozen
  stack. A dependent of a superseded change loses that edge and is gated on
  base without it. If it builds, the edge was wrong; if not, it goes `red`
  and follows its own exits.
- **Any new head returns the change to `queued`** and resets its attempts.
- **Terminal stays terminal.** Only an explicit host input (`requeue`)
  makes a new attempt, and it does so by submitting a new head.
- **Owner inputs are intent, not states.** `unstack` = `superseded(unstacked)`.
  `queue hold` and `automerge hold` set `hold_until`, which always has a
  time: 24h at most, renewed only by an explicit owner event, never by
  default. `publish` declares "no deps". `automerge off` makes the owner the
  merger, but `green` keeps its deadline: at the deadline the change goes
  into the owner's daily digest, and stays `green` with a new deadline. No
  state is human-only (owner answer 1).
- `ready` and `green` from #779 are kept as one state, `green`; #779's
  `ready` (gate passed, not published) is `landed`, which after #836 means
  "gate green on base + deps; PR open or opening".

### 3. The Reconcile contract

```go
func Reconcile(in Input) Plan
type Input struct {
    Now   time.Time     // the only clock Reconcile sees
    State State         // changes, edges, attempts, policy (from the store)
    Forge ForgeSnapshot // PRs: state, base, head, native stack, mergeable; checks; forge facts (#822)
    Git   GitFacts      // base tip, heads, patch-ids in base, ancestry, gate results per (head, onto)
}
type Plan struct{ Actions []Action } // ordered; applied one at a time
```

Actions: `Gate(change, onto)`, `Rebase(change, onto)`, `Publish(change, base)`,
`Move(pr, base, stack)`, `Link(stack)`, `Merge(pr, method)`,
`NeedsFix(change, cause)`, `Close(pr, reason)`,
`Transition(change, to, reason, deadline, retryAt)`, `Escalate(key, cause)`
and `Wait(change, reason, until)`. These are #829's set with three changes:
`Gate` is added (the host runs the repo's gate), #829's `Park` becomes a
`Transition` with `retryAt`, and #779's `retarget` is `Move`.

- **Pure.** No I/O, no clock other than `Now`, no randomness, no
  goroutines, no package-level state.
- **Deterministic.** The same `Input` gives a byte-identical `Plan`. The
  output doesn't depend on the order of input slices or on map iteration,
  and ties break on `seq`, then id.
- **Idempotent, CAS-guarded actions.** Every effectful action carries a
  guard (expected head, PR base, base tip, row `gen`). The executor checks
  it against a fresh read and applies the action with a compare-and-set:
  `git update-ref <ref> <new> <old>`, `push --force-with-lease`, read before
  every forge write, `UPDATE … WHERE gen = ?`. A failed guard is "stale,
  skip", never an error. An action whose effect is already there is a
  success. Composite forge operations (`Move` = unlink, base, relink) leave
  states that are valid inputs at every step. A crash anywhere is healed by
  the next tick. There is no journal.
- **Failures are classified, not raised.** The executor returns an
  `Outcome` (ok, transient, rate-limited until, refused with a reason,
  deterministic) that goes into `prgraph_attempt` (#826). Forge errors are
  waits, never failures.
- **Replayable.** Each tick stores `(input_hash, input, plan)` in
  `prgraph_tick`. `Reconcile(recorded input)` must reproduce the recorded
  plan exactly, and that is how a bug report is reproduced.
- **Rules inside it** (from ADR 0004, unchanged): desired base = nearest
  unmerged dependency, else base, with "builds on" checked by the gate and
  never "commits apply"; a red explained by a wrong base is `Move`, not
  `NeedsFix` (I3 before I7); `Merge` only on required, non-host checks; holds
  are computed. When a change has two unmerged dependencies that aren't in
  one chain, it gets `Wait` until all but one have merged (owner answer 2).

### 4. Invariants, each a property test

Generators build random DAGs of up to 50 changes, random snapshots and git
facts, and random executor outcomes. Each line is one `rapid`/`testing/quick`
property in `internal/prgraph`.

| # | Property | ADR 0004 |
|---|---|---|
| P1 | Determinism: `Reconcile(x)` equals `Reconcile(x)` and `Reconcile(shuffle(x))`. | I9 |
| P2 | Exits: every state in the table has a deadline and an exit, and the exit is an action, never "a human runs a command" (a table test). In a silent world (writes succeed, CI never reports, no agent pushes), advancing `Now` takes every change to `merged` or `superseded` within the DAG bound. | I5 |
| P3 | A red change holds only itself and the changes with a path of edges to it. No other change is given `Wait` because of it, whatever the train order or file overlap. | I6 |
| P4 | Every `Gate`, `Rebase` and `Publish` targets base tip plus the heads of the change's unmerged dependencies, and nothing else. No action names the integration branch. | #836 |
| P5 | Base-red: when a required check is red on base's tip, no `NeedsFix` for that check is emitted for any change; exactly one `NeedsFix(base, check)` and one `Escalate` per (base tip, check). | I7, #726 |
| P6 | `NeedsFix` causes are never a host-owned status (`saddle/base`), and never a red that a `Move` explains. | I7, I3 |
| P7 | With only transient or rate-limited outcomes, there is no `Transition` to `red` or `superseded`, no `Escalate`, and no deadline advances. | I4, #764 |
| P8 | Fixpoint: applying a plan to the model world and reconciling again yields only `Wait`. Applying any prefix of a plan (a crash) and then reconciling reaches the same fixpoint. Every effectful action has a guard. | I9, #777 |
| P9 | `Merge` of a change is emitted only when all its dependencies are merged; no `Merge` waits on a change it doesn't depend on. Merge order is a topological order. | — |
| P10 | Each published PR's desired base is its nearest unmerged dependency, else base. | I3 |
| P11 | No `Transition` out of `merged` or `superseded` without a `requeue` input; a change whose work is in base is `merged`, whatever its row says. | I1, I5 |
| P12 | Collapse is a view: `Collapsible(graph)` returns only green, dependency-closed prefixes, and it never changes the plan. | — |

Golden cases sit next to the properties: 2026-11-05 (#814 ends on t396's
branch, no `NeedsFix` for t402/t403), #726 (one base repair), #751 (no
17-deep chain), #651 and #662 (a repair is a new head on the same change).

### 5. The package boundary, for extraction to sddl

- `internal/prgraph` imports only the standard library and
  `database/sql`. A test fails if it imports any other `internal/...`
  package, or if its identifiers or strings mention agents, tasks, tmux,
  Claude, adapters, notices or decision cards. `Change.ID` is an opaque
  string.
- It defines its own input types (`ForgeSnapshot`, `GitFacts`, `Outcome`).
  Saddle's adapters convert from `internal/forge` (#822) and `internal/gitx`,
  so prgraph never depends on saddle's forge.
- Saddle implements three interfaces:
  ```go
  type Source interface {
      Snapshot(ctx context.Context, changes []ChangeRef) (ForgeSnapshot, error)
      GitFacts(ctx context.Context, changes []ChangeRef) (GitFacts, error)
  }
  type Executor interface { Apply(ctx context.Context, a Action) Outcome }
  type Locker   interface { Lock(ctx context.Context) (unlock func(), err error) }
  ```
  prgraph ships `Engine{Store, Source, Executor, Locker}.Tick(ctx)`: read,
  reconcile, apply one action under the lock, record, repeat. Saddle's
  executor decides *how*: `Gate` runs the repo's gate in a throwaway
  worktree (heavy-run queue), `NeedsFix` messages the live agent or spawns a
  repair through `App.Spawn` (#828), and `Escalate` files the blocker issue
  and the orchestrator notice. The shadow executor only records.
- `Engine.Tick` is driven by the sentinel tick in `saddle up` or the plugin
  engine, and by `land`, `prs` and `restack` as "reconcile now" (ADR 0005:
  nothing assumes the loop runs).
- Extraction (#779 step 4) moves `internal/prgraph` to sddl's module
  unchanged, plus a git executor and a `gh` source. It happens only after
  #836 has run saddle for a while.

### 6. Rollout: shadow, replay, then cutover in order

1. **#829.** The package, the properties above, the golden cases. It
   changes no behaviour.
2. **#830, shadow.** An adapter builds `Input` from today's stores. A
   supervised watcher runs `Reconcile` every sentinel tick with the
   recording executor and logs `reconcile_shadow` events: actions only
   prgraph would take, and legacy actions prgraph wouldn't. `saddle
   reconcile --plan` prints the plan; `saddle reconcile --replay <db>`
   replays a copy.
3. **Replay corpus.** quark's `~/code/quark/.saddle/state.db` (105k
   events) and saddle's own (73k), always copied into the scratch root and
   opened read-only. Old databases have no recorded snapshots, so the replay
   rebuilds inputs from events and judges **outcomes**, not exact actions:
   repairs per task (quark legacy 31%, target under 10%), repairs of repairs
   (target 0), at-risk spells (#775: 73 + 39, target 0), stacks frozen by an
   unrelated layer (#771, target 0), repairs of `saddle/base` (target 0).
   Scrubbed slices of the golden incidents are checked into
   `internal/prgraph/testdata/`; the databases themselves are not.
4. **#831, soak.** 50 changes, faults injected, zero human actions. It gates
   every cutover.
5. **Cutover, one action class at a time,** each behind `[reconcile] own`,
   each after 7 clean shadow days for that class and a green soak:
   #832 `Publish`/`Move`/`Link`/`Rebase` (delete `pr-bases.json`,
   `published.json`, `stacks.json`) → #833 `Merge` (delete `automerge.json`'s
   stop state) → #834 `NeedsFix`/`Escalate` (delete `ci-red.json`,
   `prgate.json`'s hold) → #835 computed holds (delete `stack-at-risk.json`,
   `stuck-stack.json`, `stall-held.json`, `sentinel_ack`) → #836 `Gate` on
   base + deps, the queue moves into `prgraph_change`, and
   `saddle/integration` retires. When a class is owned, its legacy code path
   is **off**, not just shadowed: two actors never write the same PR. Each
   cutover imports its files once and keeps a `.bak` for one release
   (ADR 0004's rollback).

### 7. The open tickets, reviewed against this

**Moot once the named step lands** (don't fix separately unless one hurts first):

| Ticket | Why it goes away | Close with |
|---|---|---|
| #770 automerge latch | no `Stopped`; #843 already fixed the latch | now, if #843 holds |
| #821 `saddle/base` red after repairs | host-owned checks are never red (P6); #843 fixed it | now |
| #752 repairs for a lower layer's cause | `Move` before `NeedsFix`; repairs never cut from integration | #834 |
| #651 repair held by the layer it repairs | a repair is a new head of the same change | #834 |
| #662 stacked base conflict unrepairable | `Move` via `forge.MovePR`; the executor pushes the new head | #832, #834 |
| #726 base-wide red | base-red gives one base repair (P5) | #834 |
| #766, #769 route-around / repair cap | `red` exits to `superseded(gave_up)`; dependents re-gate | #834, #835 |
| #775, #776 at-risk drift, stranded held layers | holds are computed | #835 |
| #845 sentinel labels by train order | the sentinel's flags are deleted (P3); a small fix only if it bites first | #835 |
| #778 repair bookkeeping scans events | repair state is `prgraph_attempt` | #834 |
| #705 killed tasks stay queued | killed → `superseded(killed)`; work in base → `merged` (P11) | #836 |
| #31 continuous pre-rebase | `Rebase` of `queued` changes is ordinary reconcile | #836 |
| #863 rename the landed list | `landedStack()` is deleted | #836 |
| Blocker instances #564, #597, #610, #612, #629, #661, #660, #695-#704, #728-#730, #837 | incidents of the stuck-stack and ci-red classes this removes | #834, #835 (owner answer 5) |

**Move under prgraph** (still needed, built as part of a step):
#800 (gate pass stamps = `prgraph_gate`, in #836); #754 (merge order and
plan output = `saddle reconcile --plan`, in #830); #862 (rename only the
code names that survive #835); #851 (the MCP `unstack` becomes the same
`superseded(unstacked)` input as the CLI, in #835; fix now only if it
bites); #27 barrier tasks (a barrier is a change every later change has a
`declared` edge to).

**Remain, outside the PR graph:** #822-#828 (prerequisites in ADR 0004's
plan); #795, #839 (spawn policy, #828); #808, #809 (rate limits and
transient errors: P7 relies on them); #849 (kill cancels a running done
gate: task lifecycle, #827; prgraph already ignores a gate result for a
stale head); #732, #727 (gate environment: more urgent after #836, since
every gate runs in a fresh worktree); #753 (PR body content, the host's
`Publish`); #330, #29 (structural merge, tried by the host's `Rebase`
before `NeedsFix(conflict)`); #733, #860 (orchestrator); #799, #801,
#802, #803 (hygiene; #802 deletes `saddle/integration` after #836); #804
(workers); and the unrelated #1, #3, #4, #12, #17, #24, #44, #48, #51, #78,
#165, #460, #462, #547, #641, #642, #725.

## Alternatives considered

- **Keep the epic's state names (`parked`, `dropped`).** Rejected: both
  clash with settled glossary words, and #771/#776 were bugs caused by one
  subsystem reading another's word.
- **Store the whole state machine and drive it by events.** Rejected:
  that is today's design, where a missed event leaves a wrong row forever.
  States here are recomputed every tick, and the row is a cache plus a clock.
- **A repair as its own change stacked on the red one.** Rejected: that
  is #651's trap and the source of repairs of repairs.
- **File overlap as a dependency** (as #779 wrote it). Rejected by
  ADR 0007. After #836 a change is gated on base plus its edges, so overlap
  with an unrelated red change can't break it.
- **prgraph in its own database file now.** Not yet: one `state.db` keeps
  backups, `saddle migrate` and the replay corpus in one place. The
  `prgraph_` prefix and an injected `*sql.DB` keep the split cheap for sddl.
- **One integration PR as the default** (sandcastle input 5 on #779).
  Rejected for now: stacked PRs per change stay the default (ADR 0007,
  owner answer 3). It fits as a view over a green, dependency-closed
  prefix (P12), so it can be added later without changing the engine.

## Consequences

- One schema and one machine replace about 12 files, five hand-rolled stop
  states and the `sentinel_ack`/`restack --abort`/`automerge on` hatches.
- `folded`, `repairing` and the integration branch's ranges go. #836
  rewrites the glossary entries for *landed*, *layer*, *repair*, *folded*,
  *repairing* and *superseded* (adding the `gave_up` reason).
- `prgraph_tick` costs disk; it is pruned after 14 days and by GC (#802).
- Gating each change on base + deps runs more gates than one integration
  tip. The `prgraph_gate` stamp and the heavy-run queue pay for it.
- #829 must adopt this ADR's action list (`Gate`, `Transition` instead of
  `Park`) and state names before it starts.

## Owner answers (proposed, owner to confirm)

The owner answered the open questions on 2026-11-06. They are recorded
here as proposed until the owner accepts this ADR.

1. **Automerge off:** `green` keeps a deadline whose exit is a line in the
   daily digest. No state is human-only.
2. **Two unmerged dependencies in different stacks:** `Wait` until one
   merges.
3. **Default mode:** stacked PRs per change (ADR 0007) stay the default.
4. **Deadlines:** the defaults in §2 stand. Holds are never indefinite:
   24h at most, renewed only by an explicit owner event.
5. **Stale blocker issues:** they stay open until #834 and #835 land, and
   close with them.
6. **Gate concurrency:** landing stays serial (ADR 0008). Revisit after
   the cutover.
7. **#832:** the owner reopens it; it is still the publish cutover.
