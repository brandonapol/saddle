# Bench: collision scenarios and a scorer

Status: **Proposed** (#599). The fixture, six scenarios, the fake-agent
runner and the scorer are implemented; real-agent runs and symbol
granularity are not (see [Not yet](#not-yet)).

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

## Running it

```
saddle bench list
saddle bench run control
saddle bench run same-function --out bench/results
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
6. Writes `<out>/<scenario>-<timestamp>.json` (the event log and the
   report) and `<scenario>-<timestamp>.md` (the summary), and prints the
   summary.

Flags:

| Flag | Default | Meaning |
| --- | --- | --- |
| `--adapter` | `fake` | `fake` runs the scripted agent. `claude`, `grok` and `codex` are reserved for real-agent runs, not wired yet. |
| `--granularity` | `file` | `file` is today's path claims. `symbol` is reserved for symbol-level claims (#329). |
| `--out` | `bench/results` | where the JSON and markdown go |
| `--stall` | `60s` | end the run when no task makes progress for this long |
| `--timeout` | `10m` | end the run regardless |
| `--keep` | off | keep the world's temp dir for debugging and print its path |

Fake runs are cheap and deterministic in what each agent writes, so they can
run in CI. The end-to-end test `TestBenchRunsControlAndACollision` (build tag
`e2e`, needs tmux) runs `control` and `narrow-claim`. `make test/e2e` doesn't
list `./internal/bench/...` yet (#605); run it with
`go test -tags 'e2e grammar_subset grammar_subset_go grammar_subset_python' ./internal/bench/`.
Real-agent runs spend quota, are manual and opt-in, and never run in CI.

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
would; a denied write is logged and skipped, and the agent carries on. That
is how a ticket whose claim is too narrow loses work.

`wait_for = [N, …]` holds a ticket's agent after its commit, before done,
until the agents of tickets N… have finished their scripts. The runner then
messages it `bench-release`. This pins an interleaving that would otherwise
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

Planned next (#605): a signature change with a caller elsewhere (stale
assumption), a CI-red layer under a stack, a generated file, and a serial
path (`go.mod`).

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
| `needed_help` | tickets not landed when the run ended, or with a `train_escalated` or `hand_fix` event |
| `peak_parallelism` | most tasks live at once (spawn to landed) |
| `safe_parallelism` | from scenario.toml |
| `forecast` | expected and predicted overlap, predicted and actual collision pairs, hits, misses and predicted pairs that never collided |

A *predicted collision* is a forecast pair at level medium or above. An
*actual collision* is a pair of tickets where one task's trouble names the
other:

- a `deny` of path p: every other ticket whose claims cover p;
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
- **Symbol granularity.** `--granularity symbol` waits on symbol-level
  claims (`docs/symbol-claims.md`, #329).
- **More scenarios**, listed above.
