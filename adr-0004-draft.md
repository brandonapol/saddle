# ADR 0004: Reconcile saddle's state against GitHub; no failure latches

- Status: Proposed
- Issue: #820 (this decision). Implementation: #822–#836, in order.
- Relates to: #779 (prgraph engine), #819 and ADR 0003 (landed means on main), #804 (headless workers), #805 (glossary and ADRs), the 2026-11-05 audit #763–#805.

## The page the owner reads

**Why this keeps happening.** Saddle acts on its own *model* of GitHub,
git and its tasks, and that model is never checked against the real thing.
The model lives in about 12 JSON files, the `train` table, a local
integration branch and per-feature flags. When the model is wrong, the
action fails. Each feature then has its own reaction, and most of those
reactions stop: a flag that a human must clear, a default that fails
forever, a give-up. There is no component whose job is to notice that
"what saddle thinks" differs from "what GitHub says" and repair the
difference. Each new incident added another special case: another
sentinel, another card, another escape hatch. That turned one missing
design element into about 40 bugs.

**What ends the class.**
1. **GitHub is the truth; saddle's stores hold only intent.** Every tick,
   one reconciler reads the actual state (PRs, bases, native stacks,
   checks, merged-ness, git ancestry and patch-ids). It compares that with
   the desired state (changes, their dependencies, the owner's policy) and
   emits idempotent actions. Wrong local state can't persist past one tick.
2. **No failure latches.** Every action failure is classified (transient,
   rate-limited, refused by a known forge rule, or deterministic) and
   retried on a schedule. A deterministic failure escalates **once** and
   keeps retrying slowly. Nothing waits for `automerge on`, `sentinel ack`
   or `restack --continue`.
3. **The model of GitHub is tested against GitHub.** A recorded-fixture
   contract suite runs in `make check`, and a nightly canary runs it
   against a real repo. The fake (fakegh) must pass the same suite, so it
   can't drift again.
4. **Every invariant has an owner, a detector and a heal.** That is the
   table below. A violation is an event and a notice within one tick, not
   something a human notices the next morning.

**What it replaces.** The integration branch as a destination, the
restack journal, the sentinel's at-risk flag, stuck-stack and route-around
cards, `stall-held.json`, `ci-red.json`, `pr-bases.json`,
`published.json`, `stacks.json`, automerge's `Stopped` state, and the
"repair anything red" ci-red path. It subsumes 17 open audit tickets.
Another 21 stay needed, because they're about workers or infrastructure,
not the PR graph.

## Context: the evidence, clustered by root cause

Sources: the audit issues #763–#805 (mostly quark's 105k-event
`state.db`), saddle's own `.saddle/state.db` (73,674 events,
2026-10-01 to 2026-11-05), the last 100 PRs, and the incidents of
2026-11-05 in #820 and #819. Event ids below are from saddle's `state.db`.

### 2026-11-05, end to end (the case every cluster shows up in)
- 07:04 to 12:56: seven tasks (t392, t394–t400) `landed` on
  `saddle/integration`. They were reported done, and none had a PR (#819).
  Seven PRs were opened by hand at 13:09–13:10 (#810–#816).
- 13:10:03 `pr_layout`: t397 "published as … **on main**" (event 73271),
  because its commits *applied* to main. At 13:26:55, `ci_red_sibling t396:
  symbol logErr` (73490): t397 used `logErr` from t396, so #814 needed
  #812 below it. The pre-publish gate, which would have caught that, is
  `off = true` in `.saddle/config.toml`. It was a hand fix from 2026-10-08
  (#274), still marked "REMOVE once #274 lands".
- 13:19:20 and 13:21:22 `restack_retarget #814 → main` (73356, 73399).
  Restack reported "retargeted 7 PRs" and left #814 wrong. Moving it is
  refused by GitHub ("part of a stack"), and `setPRBase`
  (`internal/app/prbase.go:176`) treats that refusal as "the relink fixes
  it", which is false across stacks.
- 13:29:53 `automerge_failed` on #811: *"This pull request is part of a
  stack and must be merged using the asynchronous merge REST API"* (73601).
  Auto-merge latched off ("auto-merge stopped after a failure", 73603 and
  on). It still shows as `Blocked` in `saddle status`.
- 13:26:34 to 13:33:56: `ci_red saddle/base` on #812 and #813 (73461,
  73469). `saddle/base` is a status **saddle itself** sets red on every
  stacked PR: "targets X, not main: merge the PRs below first"
  (`internal/app/stack_comments.go:30`, #751). The ci-red watcher read it as
  a CI failure and spawned repairs t402 and t403. Both were picked as
  `adapter=grok (suits standard, … 0 running)` (73466, 73627), although
  the owner had excluded grok and the orchestrator spawned only with
  `adapter=claude (requested)`. Both were killed. Saddle then filed #821,
  "saddle/base still red on t396's PR after 2 repairs", as a **hard**
  blocker for the owner, about its own status.
- 10:46 to 11:44: `watchdog_stall` toggled `unknown` ↔ `overload` 7 times
  in 9 minutes (72720–72752), opening and resolving a card each time.

### C1. Saddle's model of GitHub is wrong, and untested against GitHub
| Assumption | Reality | Evidence |
|---|---|---|
| `gh pr merge` merges any green PR | Stacked PRs need the async merge API | 2026-11-05 #811 (73601) |
| A stacked PR's base can be edited | Refused "part of a stack"; must unlink first | #814; `prbase.go:176` swallows it |
| A relink fixes any refused base | Only within the same stack | #814 stayed on `main` |
| "Commits apply on main" ⇒ PR can target main | Build dependencies don't show in patches | #814 needed `logErr` (73490) |
| A red status is a CI failure to repair | `saddle/base` is saddle's own layout signal | t402/t403, #821 |
| Labels exist | 874 + 729 `needs-human not found` failures | #787 |
| Rate limits are per repo and rare | Per user; one repo's watcher starves others | #763 (26,267 `ci_error` here, 30,026 in quark), #764 |
| Base changes always succeed | Refused while in a merge queue | #770 |
| `gh` calls return | Hang for hours on dead TCP; `signal: killed` | #788; 72474, 73218, 73247 |

The fake confirms each wrong model: fakegh merges stacked PRs through
`gh pr merge` (`internal/e2e/fakegh/cli.go:264`), edits stacked bases
freely (`cli.go:228`) and never rate-limits. CI was green for every
behaviour that broke.

### C2. Local state disagrees with GitHub and git, and nothing reconciles it
- The integration branch is a second "main" and a false finish line:
  seven PR-less "done" tasks (#819). Its stale commits after a layer
  leaves set off at-risk flags (#775: 73 "commits from other tasks" + 39
  "does not descend"). Repairs cut from it inherit other tasks' work (#779:
  31% of quark tasks were repairs, 29 repairs of repairs).
- About 12 JSON files plus the `train` table, written by several processes
  without a shared lock (#779: `SetCIRed` from 13 sites, a lost update
  re-adds a cleared red).
- Rows that outlive reality: landed PRs polled forever (#763); 56
  `repairing` rows with dead repairs (#772); held layers stranded after the
  red closed (#776, still in `stall-held.json`); `after` deps on PRs that
  will never merge (#774); `failed` tasks blocking their issue (#797); a
  killed task's issue read as taken (#767).
- Work stranded and recovered by hand: 9 of the last 100 PRs are "Finish
  #N: land tX's … (take over tX)" (#616, #617, #633, #634, #655, #673,
  #683, #706, #707).

### C3. One failure latches
| Latch | Evidence |
|---|---|
| automerge `Stopped` until `saddle automerge on` | #770: 837 `automerge_idle` in quark, ~71h off; 184 here; #811 today |
| decision default fails every cycle, forever | #766: 290; #787: 874 + 729 |
| resume give-up, permanent and re-logged | #782: 1,086 in quark; 242 `resume_gave_up` here |
| repair cap parks with `needs-human`, freezing the stack | #769: t552…t591 frozen 19–22h |
| failed repair spawn never retried on the same head | #768: gaps of 2.2–8.2h; #795: t63 failed 20× over 131h |
| autopilot "queue empty" turns itself off | #767, #733: 8 `autopilot_stopped` here |
| watchdog posts once, defaults to "nothing" | #783 |
| interrupted restack journal blocks every lock user | #777 |

### C4. Guards and freezes that are too wide or need a human
- At-risk freezes everything later in train order, including independent
  PRs on main (#771; spells of 712 and 1,731 min).
- `needs-human` on a bottom layer stops collapse above it (#769).
- `needs_you` has no timeout (#786); `paused` resumes only at startup
  (#790); the hooks flock waits up to 40 min, then runs unlocked (#799).
- Hand fixes with no expiry become permanent guards: `[train.prepublish]
  off = true` since 2026-10-08.

### C5. Policy applied on some spawn paths, not all
- Repairs ignore the owner's adapter exclusion (t402 and t403 on grok
  today; repairs routed to grok while it returned 402, #796). Config can
  exclude models (#482) but not adapters, so the exclusion lived only in
  the orchestrator's prompts.
- Repairs give away their claims before spawning (#795) and lose free slots
  to new tickets (#768).

### C6. Worker state inferred from the screen (out of scope here; #804)
#780, #781, #782, #786, #790, #798. The reconciler doesn't fix
screen-scraping. Invariant I5 still applies to it, and #804 removes the
inference.

### C7. Unbounded, unsupervised infrastructure (prerequisites, mostly in flight)
#788 (timeouts, PR #813), #789 (supervision, PR #816), #764 (governor,
PR #812), #778 (full event scans), #802 (GC).

## Invariants

| # | Invariant | Enforced by | Violation detected by | Heals by |
|---|---|---|---|---|
| I1 | A task is done only when base contains its work (patch-id or `Saddle-Task` trailer). "Landed" is an internal step, never a report. | prgraph desired state: every gated change has a PR moving toward merge (#829, #836) | Monitor `done_not_on_main` (#825) | `Publish` action |
| I2 | Local state is a cache of GitHub and git, refreshed every tick. Stores hold only intent (deps, policy, holds) and history. | prgraph reads `forge.Snapshot` + `GitFacts` each tick (#829) | Monitor `pr_drift`; shadow diffs (#830) | Next reconcile overwrites the cache |
| I3 | Each PR targets its nearest unmerged dependency, else base, and its head builds there. "Commits apply" is never enough. | Desired-base rule + mandatory pre-publish build (#829, #832) | Monitor `pr_base_wrong` | `Move` (unlink, base, relink via `forge.MovePR`, #822) |
| I4 | No failure latches. Every action failure is classified, retried on a schedule, and escalated at most once, while retries continue. | `internal/retry` (#826) | Monitor `latched` (any stop with no `retry_at`) | The retry schedule; a reset on input change |
| I5 | Every non-terminal state (task status, card, hold, train row) has a timeout and an automatic exit. Terminal never returns to live, except an explicit requeue or resume. | Store transition table (#827); prgraph `Park(retryAt)` | Monitor `state_without_exit` | The declared exit for that state |
| I6 | A red, refused or parked change holds only itself and its true dependents. | Computed holds (#835) | Soak assertion (#831) | Dependents re-target past it |
| I7 | Saddle never repairs its own signals. A repair needs a required, non-saddle check red at the head on its desired base and not red on base. | `NeedsFix` rule (#834) | Monitor `self_repair` | No spawn; base-red gives one blocker; wrong base gives `Move` |
| I8 | Every spawn path applies the same policy (adapter and model exclusions, cap and priority, claims, budget). Exclusions are durable config. | `App.Spawn(SpawnRequest)` choke point (#828) | Monitor `policy_bypass` | Refuse and re-pick |
| I9 | Every action is idempotent and resumable: CAS on refs, read-before-write on the forge, no blocking journal. | prgraph action contract (#829) | Soak: engine killed mid-reconcile (#831) | Reconcile again |
| I10 | Every forge behaviour saddle relies on has a contract test, run on fixtures in CI and live nightly. fakegh passes the same suite. | #823, #824 | Nightly canary diff opens an `incident` issue | Re-record fixtures, fix the model |
| I11 | Every external call has a deadline, and every loop is supervised and restarted. | #788, #789 (in flight) | `watcher_tick` heartbeats in status and doctor | Supervisor restart; typed timeout leads to retry |

## Decision: one reconciler

### Shape
```
desired  (sqlite, intent only)   change{id, branch, deps[declared|inferred|verified], policy{automerge, hold_until, dropped}}
actual   (read every tick)       forge.Snapshot{PRs: state, base, head, native stack, checks{required, saddle, other}, mergeable}
                                 + forge.Facts{stacked PRs, async merge, merge queue, required checks}
                                 + GitFacts{heads, patch-ids in base, ancestry, builds-on(base) from the pre-publish gate}
attempts (sqlite)                retry state per (action, key)

Reconcile(desired, actual, attempts) → []Action      // pure, deterministic, replayable
Action = Publish | Move | Link | Rebase | Merge | NeedsFix | Close | Park(retryAt) | Escalate | Wait(reason)
```
The engine runs `Reconcile` every sentinel tick and whenever a forge or git
change is seen. It applies actions one at a time under the train lock, each
CAS-guarded, and feeds every outcome to the retry primitive. `prs`,
`restack`, `land` and `automerge` become "reconcile now" plus policy
switches. The escape hatches stay, as **inputs**: `unstack` = dropped,
`queue hold` = hold_until, `publish` = no deps. None of them is a separate
state machine with its own stuck states.

### Rules the old code got wrong
- **Base:** nearest unmerged dependency. A dependency is *verified* when the
  head doesn't build on the candidate base and does build with it. This
  fixes #814.
- **Merge:** green on required checks only. Saddle-owned statuses describe
  the layout and are never "red CI". Stacked PRs merge by the path
  `forge.Facts` names (async API or `gh stack merge`). This fixes #811.
- **Repair:** only per I7. Wrong base and base-red are not code bugs. This
  fixes t402/t403 and #821.
- **Holds:** computed from the graph, never stored (#771, #775, #776).

### What it replaces
| Today | After |
|---|---|
| `saddle/integration` as the land target and repair source | Gate on base + deps; repairs cut from the desired base (#836) |
| `prs` layout by "commits apply", `pr-bases.json`, `published.json`, `stacks.json` | `Publish`/`Move`/`Link` from prgraph (#832) |
| restack + `restack-journal.json` + `--continue/--abort` | `Rebase` actions, CAS, no journal |
| automerge loop + `Stopped` + `automerge.json` | `Merge` action + retry primitive (#833) |
| ci-red watcher, `ci-red.json`, prgate hold, repair cap park | `NeedsFix` per I7, `Park(retryAt)` (#834) |
| sentinel at-risk flag, `sentinel_ack`, stuck-stack card, route-around, `stall-held.json` | Computed holds (#835) |
| Per-feature give-ups (`resume_gave_up`, decision default loops, autopilot off) | One retry-and-escalate primitive (#826) |

### How it relates to the epics
- **#779 (prgraph engine).** This ADR is #779's step 1 (its ADR), widened:
  #779 designed the PR-graph engine, and this adds the invariants outside the
  graph (I1, I5, I7, I8, I10), the contract tests, and GitHub as the source
  of truth for actual state. #829–#835 are #779's steps 2–3. #779's
  extraction to `sddl` (step 4) is unchanged and still comes last.
- **#819 (landed means on main), ADR 0003.** ADR 0003 makes landing open
  the PR, reports delivery (`landed locally` until merged), and has the
  sentinel's `PublishLanded` heal PR-less landed tasks. That is I1's
  enforcement *today*. It keeps `saddle/integration` as an internal detail
  of the train (option b), because the train's ranges live on it and moving
  them is the prgraph work. This ADR is that work: once prgraph owns
  changes, deps and ranges (#829–#835), #836 removes the branch (option a),
  and ADR 0003's option b becomes #836's rollback. `PublishLanded` becomes
  the reconciler's `Publish` action.
- **#804 (headless workers).** Orthogonal and complementary. The reconciler
  owns the PR graph; #804 owns the worker lifecycle. Both rely on I5 (the
  state machine in #827). #804 removes the screen-scraped inputs to it and
  makes #780, #781, #782, #786, #790 (risky prompt) and #798 moot.
- **#805 (glossary, ADRs).** This ADR fixes the words: *landed* is internal
  and never a report; *merged* means on base; *hold* is always computed;
  *stack* means a native GitHub stack of PRs; *integration branch* is
  retired. #805's GLOSSARY.md must adopt these, and #836 rewrites the
  entries.

## Audit tickets: subsumed vs still needed

Ranked by how much stall time each caused. "Subsumed" means: don't fix
separately unless it hurts before its step lands; close it when the step
lands.

**Subsumed by the reconciler (close when the named step lands)**
| Rank | Ticket | By |
|---|---|---|
| 1 | #770 automerge latches off | #826, #833 |
| 2 | #766 route-around fails forever | #834, #835 |
| 3 | #769 repair-cap freezes the stack | #834 |
| 4 | #771 at-risk freezes everything | #835 |
| 5 | #775 sentinel never restacks | #835 |
| 6 | #773 gate-red never repaired | #834 |
| 7 | #768 repair spawn not retried | #826, #828, #834 |
| 8 | #795 repair releases claims first | #828 |
| 9 | #772 no_change repair stuck `repairing` | #834 |
| 10 | #774 waiting on a dead `after` dep | #829, #835 |
| 11 | #776 held layers stranded | #835 |
| 12 | #777 restack journal blocks | #829 (no journal) |
| 13 | #784 killed tasks resurrected | #827 |
| 14 | #783 watchdog posts once, does nothing | #826, #827 |
| 15 | #790 card defaults don't do what they say | #826, #827 (default = declared exit) |
| 16 | #782 resume give-up permanent | #826 (plus the heartbeat half, still needed) |
| 17 | #778 repair bookkeeping scans events | moot once repair state is in prgraph's tables (#834) |

**Still needed (not about the PR graph, or a prerequisite)**
| Rank | Ticket | Why it stays |
|---|---|---|
| 1 | #819 landed means on main | Stopgap for I1 until #836 (in flight, t401) |
| 2 | #764 rate-limit governor | Prerequisite: becomes `ErrRateLimited` in #822 (PR #812) |
| 3 | #788 timeouts | Prerequisite I11 (PR #813) |
| 4 | #789 supervision | Prerequisite I11 (PR #816) |
| 5 | #733 / #767 autopilot retry | Autopilot's issue scan, not the PR graph (PR #814) |
| 6 | #787 label creation | Small and in flight (PR #811); #822 generalises it |
| 7 | #765 zombie worktree | Worker lifecycle; feeds #827's exits |
| 8 | #781 windowless running | Worker lifecycle (#827, then #804) |
| 9 | #786 needs_you timeout | Worker lifecycle (#827, then #804) |
| 10 | #794 ask_owner cards never close | Card/message linking |
| 11 | #796 adapter bounce / #798 expired login | Adapter health; #828 consumes it |
| 12 | #785 gate env failures | Gate environment |
| 13 | #791, #792, #793 orchestrator wake-ups | Orchestrator delivery |
| 14 | #797 spawn/checkpoint race | Watcher locking |
| 15 | #799 hooks flock, #800 duplicate gate | Gate throughput |
| 16 | #801, #802, #803 scratch, GC, worktrees | Hygiene; #802 deletes `saddle/integration` after #836 |

Already done: #763 (PR #810), #780 (PR #806).

## Migration plan
1. **Now (no behaviour change):** finish the in-flight prerequisites (#819,
   #764, #788, #789). Ship #822 (typed forge outcomes) and #825 (invariant
   monitor, detection only), so violations show up from day one.
2. **Parallel hardening:** #826 (no latches), #827 (state machine), #828
   (spawn policy). Each is useful on the legacy code and needed by the
   reconciler.
3. **Prove the model:** #823 fixtures and #824 canary. #829 prgraph,
   #830 shadow on saddle and quark, #831 soak.
4. **Cut over one action class at a time**, each behind
   `[reconcile] own = ["publish", "merge", "repair", "holds"]`, each only
   after the shadow has run clean for that class for 7 days and the soak is
   green: #832 publish/bases → #833 merge → #834 repair → #835 holds. Each
   cutover imports and then deletes its JSON files.
5. **Retire the integration branch** (#836), and rewrite GLOSSARY and
   ARCHITECTURE.

## Rollback
- Each cutover is a config switch: removing a class from
  `[reconcile] own` returns that class to the legacy code, which stays in the
  tree until two releases after its cutover. prgraph's import is
  one-way-safe: JSON files are deleted only by a later release, and until
  then the import keeps a `.bak`.
- The shadow (#830) and monitor (#825) have no side effects and can stay on
  during any rollback. That's how a rollback is diagnosed.
- #836's rollback is `[train] integration = "saddle/integration"` for one
  release (prgraph still publishes). The backups under
  `refs/saddle/integration-backups/` are kept.
- What can't be rolled back: issues and PRs saddle opened or closed. Every
  forge write is recorded as an event with enough detail to undo it by hand.

## Contract tests against real GitHub (I10)
- **Fixture suite** (#823): `internal/forge/contract`, a table of scenarios
  (setup → action → expected typed outcome → expected state). Each runs
  against recorded transcripts (`testdata/contract/`) and against fakegh,
  which must agree. It runs in `make check`. The first scenarios are the ones
  that burned saddle:
  - **Native stacks:** link, view, and merging the bottom (what happens to
    the PR above).
  - **Async merge:** a plain merge of a stacked PR is refused; the async
    merge is accepted and settles later (`ErrPending`).
  - **Base-change refusal:** a stacked PR refuses a base edit; unlink, edit,
    relink works. Also the merge-queue refusal (#770).
  - **Rate limits:** primary and secondary limit stderr and headers, and
    `rate_limit` reset parsing (#764), without exhausting a real quota.
  - **Required checks:** a failing required check blocks merge; a status
    saddle sets counts only if branch protection requires it; `gh pr checks`
    exit codes 8 and 1.
  - **Squash merges:** `mergedAt` is set, and the head is not an ancestor of
    base, so "in base" needs a patch-id or trailer.
  - **Labels:** adding a missing label (#787).
- **Nightly canary** (#824): the same scenarios live against a throwaway
  repo with Stacked PRs and a required check, from a scheduled workflow.
  `make contract-record` refreshes the fixtures. A changed outcome fails the
  job and opens one `incident` issue. This is how saddle hears that GitHub
  changed (Stacked PRs is a preview) before an overnight run does.
- **Soak** (#831): 50 changes through the contract-checked fakegh, with
  faults injected, and zero human actions. It gates every cutover.

## Consequences
- Fewer states and files. Most escape hatches become graph edits, and
  "acknowledge" disappears.
- The forge is read every tick, which costs API calls. Snapshots are
  batched (one GraphQL query per tick, as in PR #715) and run under the #764
  governor. With #763's terminal-PR filter, the steady state is about one
  call per tick per open stack.
- The pre-publish build becomes mandatory for publishing. It costs gate
  time, which #800's pass stamps offset.
- Until #836 lands, the integration branch and the reconciler coexist. The
  shadow's divergence log is the guard against the two disagreeing.
