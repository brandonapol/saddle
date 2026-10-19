# Machine-wide resource queue

Status: Proposed (#601). Builds on the heavy-run scheduler in `docs/runq.md`
and the CPU and RAM gauge in `docs/config.md` (#600).

## Problem

The heavy-run queue (`internal/runq`) is one SQLite file per user and machine,
so saddle and other saddle repos (quark, say) already take turns for
`go-test` slots. Work that never asks the queue still runs, though: quark's
flutter pre-commit hooks, `sqlc generate`, `flutter_tester`. On an 8-core box
those drive the load to ~40, and every repo's gates time out.

Two gaps let that happen:

1. Nothing outside saddle's agents goes through the queue. A git hook in
   another repo has no reason to know saddle exists.
2. Classes are counted separately. Two slots of `go-test` plus one of
   `flutter-test` plus two `generic-heavy` can all run at once, however heavy
   each is, and the load gate only looks at CPU.

## Decision

### `saddle run` is the wrapper any tool can call

```sh
saddle run --class flutter-test -- flutter test
saddle run --class generic-heavy --max-wait 10m -- sqlc generate
saddle run --no-queue -- flutter test   # skip the queue, still run
```

It takes a lease from the machine's queue, runs the command, and releases.
It forwards stdin, stdout, stderr, signals and the exit code. It works outside
a saddle repo: only the user config (`~/.config/saddle/runq.toml`) applies
there. A run started without `SADDLE_TASK` is recorded as **foreign**: it was
not started by a saddle agent. `saddle runq status` marks foreign leases, and
`saddle queue` lists the foreign heavy runs under the merge train, so the
orchestrator can see why its gate is waiting.

### Weighted classes share one machine budget

Each class has a **weight**: how many units of the machine's **budget** one
run uses. `flutter-test` weighs 3; every other built-in class weighs 1. The
budget defaults to half the cores (at least 3, so the heaviest built-in class
can always run). A run starts only when:

1. its class has a free slot (the old per-class cap still applies),
2. the weights of the running leases plus its own fit in the budget, and
3. the load gate admits it (below).

The weight is stored on each lease row, so processes with different configs
still count each other's runs at the weight the runner asked for. A run whose
weight is over the whole budget starts only when nothing else holds a unit.

Budget ordering is strict across classes. Waiters are ranked by aged priority
then arrival, the same order as inside a class. The first waiter that doesn't
fit blocks everyone behind it, so a stream of light `go-test` runs cannot
starve a `flutter-test` waiting for 3 units. Aging still lifts a background
run above fresh worker runs over time.

### Load-aware admission uses the gauge's numbers

The load gate already holds the head of a class while load per core is over
`max_load_per_cpu` (1.0) or CPU pressure is over `max_cpu_pressure` (60%). It
now also holds while available RAM (`MemAvailable` as a percent of
`MemTotal`) is under `min_free_ram_pct`. That is the reading the gauge shows,
and the default (10%) is the gauge's `busy_ram_pct`, the point where it says
"Overloaded: don't". It still fails open on an unreadable probe.

After `gate_max_wait` (10m) the gate stops holding, but it keeps a floor: one
heavy run may start anyway, and no second one does while it holds (the wait is
still bounded by `wait_max`). A persistently overloaded box therefore runs one
heavy run at a time instead of everything at once.

### Observe mode and `saddle doctor`

The queue's default mode is `observe`: it records runs but never makes one
wait, so slots, budget and the load gate are inert. `saddle doctor` warns
while the queue is observing; set `mode = "enforce"` in `.saddle/runq.toml`
(this repo) or `~/.config/saddle/runq.toml` (the machine) when you are done
measuring.

### Compound commands and `make` targets

A gate's `test.cmd` is classified per segment: it is split on `&&`, `||` and
`;` (outside quotes), each segment is matched against the `[classes.*]`
`match` patterns, and the command takes the heaviest class among its segments
(by `weight`, then name). `flutter pub get && make check` is a flutter class
run, not `default`. A `make <target>` segment is classified by what the
Makefile recipe for that target (and its prerequisites, and `$(MAKE)` calls)
runs, when that matches a class; otherwise the `match` patterns apply as
before. A repo that wants an explicit mapping lists the command in that
class's `match`.

### Config

The repo's `.saddle/config.toml` gets a `[run_queue]` section for the
admission keys. `.saddle/runq.toml` and then `~/.config/saddle/runq.toml`
override it, key by key, since the user file describes the machine:

```toml
[run_queue]
# max_load_per_cpu = 1.0   # hold while load1 / cores is over this (0 off)
# max_cpu_pressure = 60    # ... or CPU PSI some avg10 is over this % (0 off)
# min_free_ram_pct = 10    # ... or MemAvailable is under this % of MemTotal (0 off)
# budget = 4               # machine units runs share (default cores/2, at least 3)
# [run_queue.weights]
# flutter-test = 3
# go-test = 1
```

`0` turns a check, or the budget, off. The same keys go at the top level of
`runq.toml` (`budget`, `min_free_ram_pct`) and per class
(`[classes.flutter-test] weight = 3`).

### No orchestrator-to-orchestrator messaging

Two saddle sessions never ask each other for room. A lease is a row in
shared state that any process can read, reap and count. A request/reply
protocol between orchestrators could deadlock (each waiting on the other's
reply) or race (both told "go"), and would need both to be alive. A row
needs neither.

### Escape hatches

Nothing here may block a commit for good (#119):

- **Dead holders.** A lease holds an flock on its lock file. The kernel drops
  it when the process dies, and the next poll by any process reaps the row.
  Leases from other hosts fall back to the heartbeat (`stale_after`, 15s).
- **`--no-queue`** runs the command at once without a lease. So does
  `SADDLE_RUNQ=off` for every run in a shell.
- **`--max-wait D`** waits at most D, then runs the command anyway, unqueued,
  with one line on stderr. A wedged queue costs a commit D, never the
  commit. (`--wait-max` keeps its old meaning for agents: give up with exit
  75 and report it.)
- `saddle runq kill`, `drain` and `slots` work as before.

### Opt-in for other repos

Saddle documents the line; it never edits another repo's hook config.

lefthook (`lefthook.yml`):

```yaml
pre-commit:
  commands:
    flutter-test:
      run: saddle run --class flutter-test --max-wait 10m -- flutter test
```

pre-commit (`.pre-commit-config.yaml`):

```yaml
- repo: local
  hooks:
    - id: flutter-test
      name: flutter test
      entry: saddle run --class flutter-test --max-wait 10m -- flutter test
      language: system
      pass_filenames: false
```

A plain `.git/hooks/pre-commit` script wraps its heavy line the same way.
Where `saddle` may not be installed, guard it:
`command -v saddle >/dev/null && saddle run --class X --max-wait 10m -- cmd || cmd`.

## Open questions

1. **Stale leases.** flock covers same-host processes that die. A process
   that hangs (alive, holding its lock) keeps its units until `saddle runq
   kill` or the owner's `max_run` alarm. Should a foreign lease past
   `max_run` stop counting toward the budget? Proposed: no automatic kill,
   but `saddle queue` flags it as overdue, and a later change may stop
   counting overdue foreign leases.
2. **Fairness across repos.** Ordering is per priority and arrival, not per
   repo. A repo with ten agents gets ten times the turns of a repo with one.
   A per-repo round-robin inside a priority band is possible but needs real
   contention data first (`saddle runq stats` records repo per lease).
3. **Done gates over pre-commit.** Saddle's gates run at `PrioGate` (20),
   agents at `worker` (10), and `saddle run` defaults to `worker`. A human's
   pre-commit hook in another repo and an agent's done gate both look like
   someone waiting. Proposed: keep `worker` for foreign runs; the train
   still outranks both. A hook that should yield can pass
   `--prio background`.
4. **Budget size.** Half the cores is a guess. Once `saddle runq stats` shows
   average cores per class (`cpu_ms / held_ms`), weights could be derived the
   way adaptive slots are.

## Tests

- Dead holder: `TestLeaseExpiresWhenProcessDies` (a lease held by a killed
  child process is reaped and its units freed).
- Weights: `TestBudgetCountsWeights`, `TestBudgetBlocksLighterRunsBehindHeavyWaiter`,
  `TestOverBudgetRunStartsAlone`, `TestBudgetOff`, `TestConfigWeightsAndBudget`.
- Admission under load: `TestLoadGateHoldsOnLowRAM`, `TestAcquireWaitsOnLowRAM`.
- CLI: `TestRunNoQueue`, `TestRunMaxWaitRunsAnyway`, `TestRunForeignLease`,
  `TestQueueShowsForeignRuns`, `TestForeignLeaseShowsInStatus`.
- Config: `TestRunQueueConfig`, `TestHeavyRunQueueLayer`.
