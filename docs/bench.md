# Bench: collision scenarios and a scorer

Status: **Proposed** (#599). The fixture, ten scenarios, the fake-agent
runner, the scorer and symbol granularity with a file-vs-symbol comparison
(#626) are implemented; real-agent runs are not (see [Not yet](#not-yet),
#605).

## Why

Saddle exists to run agents in parallel without them stepping on each
other. Claims, the write guard, the forecaster, restack and the merge train
all serve that, but nothing measures how well they do it on a fixed
workload. The bench is that workload: a small repo built so tasks collide
in known ways, dummy tickets that force the collisions, and a scorer that
turns one run into numbers that can be compared across saddle versions,
adapters and (later) claim granularity.

## Layout

```
bench/
  fixture/                 a small multi-package Go module (its own go.mod)
  scenarios/<name>/
    scenario.toml          expectations and the fake agent's edits
    tickets/*.md           the dummy tickets, filed as issues
  results/                 run output (gitignored)
internal/bench/            loader, planner, runner and scorer
internal/cli/bench.go      `saddle bench`
```

`bench/fixture` has its own `go.mod`, so the saddle module's `./...` skips
it. Its packages:

| Package | Holds | Collides through |
| --- | --- | --- |
| `greet` | `Hello`, `Goodbye` | nothing (control) |
| `mathx` | `Sum`, `Max` | callers in `report` |
| `shapes` | `Rect`, `Circle` with `Area` and `Perimeter` in one file | same file, same function |
| `textutil` | `Reverse`, `Title` in `util.go` | a rename |
| `server` | a `Routes` table and a handler per route | a shared registry |
| `report` | `Line`, built on `greet` and `mathx` | an edit outside its claim |
| `units` | `Scale` | a signature `label` calls |
| `label` | `Doubled`, built on `units.Scale` | the existing caller of a signature |
| `ledger` | `Post` and `View` | the bottom of a stack |
| `codec` | `Spec` and generated `Generated` | a generated file |

`go.mod` is the serial path: two tickets rewrite its `go` line.

## Running it

```
saddle bench list
saddle bench run control
saddle bench run same-function --out bench/results
saddle bench run same-file --granularity symbol
saddle bench compare                  # every scenario, file vs symbol
saddle bench compare same-file signature
```

`saddle bench run <scenario>` must run inside a saddle source checkout: it
builds saddle, the fake `gh` and the fake agent from it, the same way
`make test/e2e` does. Each run:

1. Builds a hermetic world with `internal/e2e`: a temp `HOME`, a bare
   origin that is also the fake GitHub, a private tmux server.
2. Copies `bench/fixture` into the repo as its first real commit and runs
   `saddle init`. The test command is the fixture's `go test ./...`, so the
   done gate and the merge train check real code.
3. Files every ticket as a ready issue (in ticket order) and turns
   autopilot on with the scenario's concurrency cap.
4. Ticks autopilot until it stops, nothing has happened for `--stall`
   (default 60s), or `--timeout` (default 10m) passes.
5. Checks out the integration branch and runs the fixture's tests on it.
6. Writes `<out>/<scenario>-<granularity>-<timestamp>.json` (the event
   log and the report) and `<scenario>-<granularity>-<timestamp>.md` (the
   summary), and prints the summary.

Flags:

| Flag | Default | Meaning |
| --- | --- | --- |
| `--adapter` | `fake` | `fake` runs the scripted agent. `claude`, `grok` and `codex` are reserved for real-agent runs, not wired yet. |
| `--granularity` | `file` | `file` is path claims, saddle's default. `symbol` sets `[claims] granularity = "symbol"` in the world's config and opens the bench's spawn gate (see [Symbol claims](#symbol-claims)). |
| `--out` | `bench/results` | where the JSON and markdown go |
| `--stall` | `60s` | end the run when no task makes progress for this long |
| `--timeout` | `10m` | end the run regardless |
| `--keep` | off | keep the world's temp dir for debugging and print its path |

Fake runs are cheap and deterministic in what each agent writes, so they can
run in CI. The end-to-end test `TestBenchRunsControlAndACollision` (build tag
`e2e`, needs tmux) runs `control` and `narrow-claim`. `make test/e2e` includes
`./internal/bench/...`, so that test runs with the other end-to-end journeys.
Real-agent runs spend quota, are manual and opt-in, and never run in CI.

## Symbol claims

`--granularity symbol` runs the same scenario with saddle's symbol-level
claims on (`docs/symbol-claims.md`): rule 1 lets a task write a file another
task claims when their edits touch different symbols, rule 2 denies a write
over a symbol that changed on integration since the task's base, and rule 3
warns when an edit uses a symbol whose signature another live task is
changing (and denies it once that change has landed).

Autopilot's own spawn gate stays path-based in both modes, so on its own it
would never start two tickets that claim one file and rule 1 would never
come into play. Under symbol claims the bench therefore opens the gate for
its tickets: autopilot sees the scenario's live tasks as holding no claims,
and a ticket whose claims another live task already holds is spawned
without those globs. Those files stay their owner's, so each write to them
is a path-claim deny that the symbol rules then rule on. This models an
orchestrator that leaves overlap to the edit-time rules; a symbol-aware gate
in autopilot itself is #627.

`saddle bench compare [scenario…]` runs each scenario (all of them by
default) once per granularity, writes every run's files like `run`, and
prints one table with a row per scenario and each cell as file → symbol:
tickets landed, peak parallelism (with the safe number), denied writes,
claim waits, train bounces, tickets that needed help, the integration
tests, and the symbol run's shares, denies and notes. It takes `--adapter`,
`--out`, `--stall` and `--timeout`.

`TestBenchSymbolGranularity` (build tag `e2e`) holds the acceptance:

- `same-file` reaches peak parallelism 1 under file claims and 2 under
  symbol claims, where both tickets land, the integration tests pass, and
  the second writer's deny is a symbol share.
- `same-function` under symbol claims: the second writer of `Rect.Area` is
  denied (rule 1, or the path deny it falls back to past its 250ms
  budget), recorded as the #1–#2 collision.
- `signature` under symbol claims: #2's `units.Scale(v, 2)` gets rule 3's
  note while #1's new signature is live, recorded as the #1–#2 collision.
  It is a warning, so #2 still lands and #1 then fails the train's tests,
  as it does under file claims.

What the first comparison found (fake agent, 2026-10-15):

| Scenario | Landed | Peak (safe) | Denied writes | Claim waits | Train bounces | Needed help | Integration | Symbol shares / denies / notes |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| ci-red | 2/2 → 2/2 | 1 → 1 (2) | 0 → 0 | 0 → 0 | 0 → 0 | – → – | pass → pass | 0 / 0 / 0 |
| control | 2/2 → 2/2 | 2 → 2 (2) | 0 → 0 | 0 → 0 | 0 → 0 | – → – | pass → pass | 0 / 0 / 0 |
| generated | 2/2 → 1/2 | 1 → 2 (1) | 0 → 1 | 1 → 1 | 0 → 0 | – → #2 | pass → pass | 0 / 1 / 0 |
| narrow-claim | 1/2 → 2/2 | 2 → 2 (2) | 1 → 0 | 0 → 0 | 0 → 0 | #2 → – | pass → pass | 1 / 0 / 0 |
| registry | 2/2 → 1/2 | 1 → 2 (1) | 0 → 1 | 1 → 1 | 0 → 0 | – → #2 | pass → pass | 0 / 1 / 0 |
| rename | 2/2 → 1/2 | 1 → 2 (1) | 0 → 1 | 1 → 1 | 0 → 0 | – → #2 | pass → pass | 0 / 1 / 0 |
| same-file | 2/2 → 2/2 | 1 → 2 (2) | 0 → 0 | 1 → 1 | 0 → 0 | – → – | pass → pass | 1 / 0 / 0 |
| same-function | 2/2 → 1/2 | 1 → 2 (1) | 0 → 1 | 1 → 1 | 0 → 0 | – → #2 | pass → pass | 0 / 1 / 0 |
| serial | 1/2 → 1/2 | 1 → 2 (1) | 0 → 1 | 1 → 1 | 0 → 0 | #2 → #2 | pass → pass | 0 / 0 / 0 |
| signature | 1/2 → 1/2 | 2 → 2 (1) | 0 → 0 | 0 → 0 | 1 → 1 | #1 → #1 | pass → pass | 0 / 0 / 1 |

- `same-file` reaches its safe parallelism of 2 under symbol claims and
  lands both tickets; under file claims it runs them one at a time. The
  one claim wait in both modes is the tick that spawned #1: autopilot
  counts a ticket spawned this tick against the next one.
- `narrow-claim` gets better: #2's write outside its claim lands in a
  function #1 doesn't touch, so rule 1 shares it and both tickets land.
- `same-function`, `rename`, `registry` and `generated` get worse: with the
  gate open both tickets run, rule 1 denies the second writer of the shared
  symbol, and the fake agent's script stops at a denied write, so #2 never
  lands. File claims serialize them and both land. A real agent told
  "leave it to t1" could finish around it; the fake one can't. The deny is
  recorded every time, so nothing collides silently.
- `serial` is unchanged in effect: `go.mod` is the train's, rule 1 never
  overrides a serial deny, and #2 needs help in both modes.
- `signature` warns instead of failing silently: #2 gets rule 3's note
  while #1's new `Scale` is live. It is only a warning, so #2 lands and #1
  fails the train's tests, as under file claims.

So the open gate is right for disjoint edits and costs a ticket wherever
the forecaster already says critical; a gate that consults the forecast
(#627) would keep both.

## The fake agent's edits

A real agent reads the current file and changes it. The fake agent can't,
so the runner does that for it at spawn time: it reads the task's base (the
integration branch at the moment the task is spawned), applies the ticket's
edits to it, and scripts the fake agent to write the results, commit and
call done. A task spawned after another landed sees that work, exactly as a
real agent would. Two tasks spawned together both edit the original and
meet at the merge train.

Edits are applied in order to an in-memory copy of the base:

| Edit | Effect |
| --- | --- |
| `old` + `new` | replace the first `old` with `new`. When `old` isn't in `path` (or `path` is gone), any file in the same directory that holds `old` is edited instead: the agent follows a move. When no file holds it the edit is stale: logged as a `bench_stale` event and skipped. |
| `new` alone | write `new` as the whole file |
| `append` | append `new` to the file |
| `from` | move `from` to `path` (a rename) |
| `remove` | delete `path` |

The fake agent asks the write guard before every write, as Claude Code
would; a denied write is logged and its script stops there, so the ticket
never calls done. That is how a ticket whose claim is too narrow loses
work. After a write, its PostToolUse carries `tool_response.originalFile`
(the file before the write) as Claude Code's does, so rule 3 can see what
the write added.

`wait_for = [N, …]` holds a ticket's agent after its commit, before done,
until the agents of tickets N… have finished their scripts. The runner then
messages it `bench-release`. `start_after = [N, …]` holds a ticket's agent
before its first write until the agents of tickets N… have committed (or
stopped); the runner then messages it `bench-start`. A `start_after` cycle
is rejected at load. This pins an interleaving that would otherwise
be a race (for example: the narrow-claim ticket must write while the other
task still holds the claim).

## Scenarios

| Scenario | Tickets | Forced collision | Expected overlap | Safe parallelism |
| --- | --- | --- | --- | --- |
| `control` | `greet` and `mathx` | none | none | 2 |
| `same-file` | `Rect.Perimeter` and `Circle.Area`, one file | different functions of one file | low | 2 |
| `same-function` | two changes to `Rect.Area` | the same function | critical | 1 |
| `rename` | move `textutil/util.go`; edit `Reverse` in it | a rename under an edit | critical | 1 |
| `registry` | two new routes in `server.Routes` | a shared registry | critical | 1 |
| `narrow-claim` | `mathx.Clamp`; `report` writes `mathx/mean.go` outside its claim | a write outside the claim | medium | 2 |
| `signature` | `units.Scale`'s signature; `extra.Twice` still calls the old one | a caller elsewhere keeps a stale signature | medium | 1 |
| `ci-red` | `ledger.Post`; `audit` stacks on it (`After: #1`) and imports `ledger` | a layer under a stack | medium | 2 |
| `generated` | both add a name to `codec.Spec` and its generated mirror | a generated file | critical | 1 |
| `serial` | both rewrite the `go` line in `go.mod` | a serial path | high | 1 |

*Expected overlap* is the worst pair's level from `internal/forecast`, given
each ticket's claims and the files, symbols and imports its edits touch
against the fixture. `TestScenarioOverlapMatchesForecast` keeps the two in
step: a scenario whose expectation drifts from the forecaster fails.

*Safe parallelism* is how many of the scenario's tickets can run at once
with no collision at all, judged by hand. With file claims, saddle runs
overlapping claims one at a time, so `same-file` reaches only 1 of a safe 2:
the gap symbol claims are meant to close.

What the first runs found: in `same-file` and `same-function` the second
ticket never starts. Once the first lands, saddle's #340 repair guard
refuses the spawn ("needs confirmation") because its claim covers a file a
landed task changed, and autopilot retries it forever (#604). The report
shows it as a spawn refusal and a ticket that needed help.

`signature` is the stale assumption. Each ticket's edits pass the fixture's
tests, and the two together do not
(`TestSignatureScenarioBreaksWhenCombined`). #2 starts after #1 has
committed and #1 waits for #2 before done, so #2 always writes while #1's
change is live: `extra.Twice` is written
against `Scale(v, n int)` while the other ticket has changed it to
`Scale(v int, unit string)`. Their claims do not overlap, so file claims
do not keep them apart. `ci-red` stacks the second ticket on the first
with `After: #1`. Their files do not clash, which is why its safe
parallelism stays 2, and a red check on the ledger layer is what holds
the audit ticket above it. `generated` has both tickets rewrite one
generated variable, so the forecaster calls it critical. `serial` has
both rewrite `go.mod`'s `go` line, the file only the merge train should
own.

## Metrics

The runner records an event log: saddle's own events (`.saddle/state.db`),
autopilot's reasons for holding each ticket back, the plan and the final
state. `bench.Score` turns that log into a report and is a pure function of
it (`TestScoreFixedLog`).

| Metric | From |
| --- | --- |
| `wall_seconds` | run start to end |
| `tokens` | reported by the adapter; null for the fake agent |
| `tickets`, `landed`, `all_landed` | tasks whose status is `landed` |
| `integration_green` | the fixture's `go test ./...` on the integration branch after the run |
| `denied_writes` | `deny` events: the write guard refused a write |
| `claim_waits` | tickets autopilot held back at least once because a claim overlapped a live task's |
| `train_bounces` | `train_conflict` plus `train_test_failed` events |
| `train_conflicts`, `test_failures` | each of those alone |
| `restacks`, `restack_conflicts` | `restack` and `auto_rebase` events; `restack_conflict` events |
| `stale_edits` | `bench_stale` events: an edit whose anchor was gone |
| `spawn_refusals` | tickets with an `autopilot_skip` event: autopilot tried to spawn them and saddle refused |
| `symbol_shares` | `deny` events that rule 1 turned into an allow (the task's next event is a `symbol_allow` on that path); not counted in `denied_writes` |
| `symbol_denies` | `symbol_deny` events: a symbol rule refused a write. Counted once in `denied_writes`, even when CheckWrite's `deny` came first |
| `symbol_notes` | `symbol_note` events: rule 3 warned about a signature another live task is changing |
| `symbol_fallbacks` | `symbol_fallback` events: a symbol rule could not decide and kept today's verdict |
| `needed_help` | tickets not landed when the run ended, or with a `train_escalated` or `hand_fix` event |
| `peak_parallelism` | most tasks live at once (spawn to landed) |
| `safe_parallelism` | from scenario.toml |
| `forecast` | expected and predicted overlap, predicted and actual collision pairs, hits, misses and predicted pairs that never collided |

A *predicted collision* is a forecast pair at level medium or above. An
*actual collision* is a pair of tickets where one task's trouble names the
other:

- a `deny` of path p: every other ticket whose claims cover p (not when
  rule 1 allowed the write, nor when a `symbol_deny` names the collision);
- a `symbol_deny`: rule 1's with the task it names, rule 2's with every
  other ticket whose edits touch its path, rule 3's with every other ticket
  that changes a symbol of that name;
- a `symbol_note`: the ticket of the task whose signature change it names;
- a `train_conflict`, `restack_conflict` or `bench_stale` on files F: every
  other ticket whose edits touch a file in F;
- a `train_test_failed`: every other ticket landed before it whose files
  share a package with the failing task's files or the packages they import.

A predicted pair that never collided is not necessarily a false alarm:
with file claims, autopilot serializes overlapping tickets, which is the
point of predicting them. Read it next to `claim_waits`.

## Adding a scenario

1. Make `bench/scenarios/<name>/tickets/NN-slug.md`, one per ticket: a
   `# Title` line, then the body as a GitHub issue would have it, with a
   `Difficulty:` line and a `claims:` line.
2. Write `bench/scenarios/<name>/scenario.toml`:

   ```toml
   description = "Two tickets change the same function."
   overlap = "critical"      # none, low, medium, high, critical
   safe_parallelism = 1
   concurrency = 2           # autopilot's cap; default: the number of tickets

   [[ticket]]
   file = "01-guard-area.md"
   # wait_for = [2]          # optional: hold before done until #2's agent finishes

   [[ticket.edit]]
   path = "shapes/shapes.go"
   old = "return r.W * r.H"
   new = "if r.W < 0 { return 0 }; return r.W * r.H"
   ```

   Tickets are numbered by their order here, from 1.
3. Run `go test ./internal/bench/` (the fixture passes, every edit applies
   to the fixture, the expected overlap matches the forecaster), then
   `saddle bench run <name>`.
4. If the fixture needs a new shape of code, add a package to
   `bench/fixture` with tests; the fixture's suite must pass on its own.

## Not yet

Tracked in #605.

- **Real agents.** `--adapter claude|grok|codex` would run real agents on
  the same tickets in an isolated world that still sees the user's adapter
  credentials, and report tokens. Opt-in, manual, never in CI.
- **A symbol-aware spawn gate in autopilot.** Under symbol claims the
  bench opens the gate itself (see [Symbol claims](#symbol-claims), #627).
