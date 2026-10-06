# Heavy-run scheduler (runq)

Status: design accepted for implementation; spike in `internal/runq` (#236).

## Problem

Parallel agents start CPU-heavy commands at the same time: `flutter test`,
`flutter analyze`, `golangci-lint`, `go test -race`, e2e suites, a repo's
`make check`. Each agent, the merge train's gate, the pre-publish layer
checks and the repo's own pre-commit hooks start their own run, so the box
saturates and every run gets slower. Several `saddle up` sessions, the
orchestrator, the workers and other repos on the same machine all need to
coordinate, so an in-process lock isn't enough.

## Decision

1. **A lease queue with no daemon, per user and per machine.** It lives in one
   SQLite file at `$XDG_STATE_HOME/saddle/runq.db`. Each lease also holds an
   flock on its own lock file, and that lock is how other processes tell it's
   alive. Classes have slot counts. Waiters are ordered by priority, aged by
   wait time, then FIFO.
2. **`saddle run --class C -- cmd` is the one entry point** (a queue wrapper,
   option 4(i)). It waits without polling hot, prints one status line, runs
   the command and records how long it waited and ran. It is not a CI runner.
3. **Interception is layered:**
   1. saddle's own gates call the queue directly.
   2. A PreToolUse hook rewrites the agent's known heavy Bash commands to go
      through `saddle run`.
   3. PATH shims in agent panes catch heavy tools started by git hooks and
      Makefiles.

   Re-entrancy through `SADDLE_RUNQ_LEASE` makes these layers compose
   without deadlock.
4. **Load gate.** On top of the slots, the head of a class holds while PSI or
   load average says the box is saturated by other work. It waits at most
   `gate_max_wait`, then starts anyway.
5. **Measure first.** Ship `saddle run` in observe mode (it records, never
   waits) for a week, size the slots from the data, then turn on waiting.

## How the prototype works (`internal/runq`)

```
            runq.db (WAL)                         runq.db.locks/<token>
  classes(name, slots)                            one file per lease,
  leases(seq, token, class, prio, state,          flock(LOCK_EX) held by
         label, cmd, pid, host,                   the lease's process
         enqueued, granted, heartbeat)
  history(class, label, cmd, waited_ms,           runq.db.wake
          held_ms, ended, how)                    touched on release; waiters
                                                  watch it with fsnotify
```

- **Acquire** creates and flocks `locks/<token>`, then inserts a `waiting`
  row. Each attempt runs in one `BEGIN IMMEDIATE` transaction:
  1. Reap dead leases.
  2. Heartbeat.
  3. Rank this class's waiters by `prio + wait/aging_step` (descending), then
     `seq`.
  4. If our rank is within the free slots and the gate admits us, set the
     row to `running`.

  Between attempts the contender sleeps with backoff from `poll_min` (50ms),
  doubling to `poll_max` (heartbeat/2), and the wake file cuts the sleep
  short. It never spins: `TestWaitDoesNotBusyPoll` caps a two-second wait at
  25 attempts.
- **Liveness and reaping.** The kernel drops an flock when the process
  holding it dies, SIGKILL included. Any contender's next attempt (or
  `Status`) can probe a lease's lock with `LOCK_NB`; if it gets the lock, the
  holder is dead and its row is deleted. This needs no PID check, so PID
  reuse and `/proc` parsing aren't a problem.
  - Heartbeats older than `stale_after` are the fallback for rows the probe
    can't vouch for, such as those from another host on a shared home.
  - Orphan lock files are swept: a process killed between creating its file
    and inserting its row, or between deleting its row and removing its
    file, leaves one behind.
  - `Run` sets `Pdeathsig=SIGKILL` on Linux, so a reaped holder's child
    doesn't keep burning CPU.
- **Re-entrancy.** `Run` exports `SADDLE_RUNQ_LEASE=<token>` to its child.
  A contender that finds a live, running lease under that token rides on it
  (`Nested()`), whatever its class, and its Release does nothing.
  - Because a nested run never holds one lease while waiting for another,
    nesting can't deadlock.
  - A stale token, one whose outer run already ended, queues normally.
- **Escape hatches.**
  - `SADDLE_RUNQ=off` bypasses the queue.
  - `Kill(token-prefix)` removes a holder or a waiter. The holder's `Lost()`
    closes; a waiter's `Acquire` returns an error.
  - `SetSlots` changes a class's slots on the whole machine; 0 drains it.
  - A database SQLite can't open, or one that fails `quick_check`, is moved
    to `runq.db.corrupt-<ts>` and recreated. Queue state is ephemeral, so
    losing it costs at most one round of re-queueing.
  - The load gate gives up after `GateMaxWait`.
- **Status** reaps first, then reports each class's slots, holders (label,
  pid, cmd, age) and waiters in grant order with their positions.
- **`Run(ctx, class, prio, cmd, out)`** prints a line when the request
  queues and again only when its position or holder changes:

  ```
  queued: position 1 of 2 for go-test, holder t83 (make check, 40s)
  ```

  When the slot comes, it prints `runq: go-test slot acquired after 52s`.
- `internal/runq/cmd/runq` is a throwaway CLI (`run`, `status`) for the e2e
  journeys and for trying the queue by hand.

### Spike results

All of these pass under `-race`; the e2e journeys pass three runs in a row.

| Test | What it shows |
| --- | --- |
| `TestTwoProcessesTakeTurns` | Two OS processes (the test binary re-exec'd) share one `flutter-test` slot. B prints `queued: position 1 of 1 for flutter-test, holder a (hold, …)`, starts by itself when A releases, and their run spans never overlap. |
| `TestKilledHolderFreesSlotWithinOneHeartbeat` | With a 1s heartbeat, SIGKILL the holder: the waiting process starts **~260ms** later (bound: `poll_max` = heartbeat/2), and history records the dead run as `reaped`. |
| `TestReleaseWakesWaiter` | A normal release hands off in **1–6ms** through the fsnotify wake file, even when the waiter's backoff has grown to seconds. |
| `TestNestedLeaseDoesNotDeadlock`, `TestNestedLeaseInProcess` | A run holding the only slot starts a child asking for the same class; the child rides the lease. Releasing the nested lease leaves the outer one held, and a stale token queues normally. |
| `TestPriorityOrdering` | Background, worker and gate requests arrive in that order and are granted gate, worker, background. |
| `TestAgingPreventsStarvation`, `TestFIFOWithinPriority` | Aging lets an old background request beat fresh worker requests. A holder that re-asks goes behind whoever is waiting. |
| `TestCrashSafety` | Three churning contenders are SIGKILLed at random points, four rounds. Afterwards `integrity_check` is ok, no leases or lock files are left, and a new acquire succeeds at once. |
| `TestCorruptDatabaseMovedAside` | A garbage db file is moved aside and the queue keeps working. |
| `TestLoadGateHoldsRunWhileSaturated`, `TestLoadGateEscapeHatch`, `TestProcLoad` | A fake load source holds a free slot while load per CPU or PSI is over its ceiling, and the hold shows in the status line. The gate gives up after `GateMaxWait` and fails open on an unreadable probe. |
| `TestStatusLine`, `TestRunPrintsQueueAndPassesToken`, `TestWaitDoesNotBusyPoll`, `TestSlotsPerClass`, `TestKill`, `TestBypass` | Status and the one-line UX; the child sees `SADDLE_RUNQ_LEASE`; backoff stays bounded; per-class slots and `SetSlots`; kill; bypass. |
| e2e `TestJourneyRunqTwoContendersTakeTurns` | Two real `runq run` processes share one slot. `runq status` shows `1/1 slots busy, 1 waiting`, the runs come out in order and B reports its wait. |
| e2e `TestJourneyRunqCrashedHolder` | SIGKILL the holder's `runq` process: its `sleep` child dies too (Pdeathsig) and the waiter starts within ~1 heartbeat. |
| e2e `TestJourneyRunqNestedRunDoesNotDeadlock` | `runq run -- runq run -- …` with one slot finishes without queueing. |

What the spike taught:

- **flock is the right liveness signal and SQLite the right ordering
  store.** Neither works alone (see Q2).
- **Aging has to be relative.** Every waiter ages at the same rate, so aging
  only reorders requests that arrived far enough apart. A background request
  beats a worker request that arrived `(PrioWorker-PrioBackground) ×
  aging_step` later: 10 × 30s = 5 minutes by default. That rule is what
  prevents starvation, and the test now checks it.
- **Orphan lock files are real.** The first crash test left one behind, so
  the reaper now sweeps them. `lockNew` re-checks that the locked inode is
  still the one at the path, which closes the sweep-versus-create race.

## Questions answered

### 1. Where is a heavy run intercepted?

Ranked, all of them in the end, built in this order:

1. **(c) saddle-owned runs call the queue directly.** That covers the train's
   test gate (`test.cmd`), the pre-publish layer checks (#223) and the CI-red
   repair runs, all at `PrioGate`. It is exact and needs no parsing. They are
   also the runs that block landing, so they matter most.
2. **(a) A PreToolUse hook that rewrites, not denies.** Claude Code's
   PreToolUse hook can return an `updatedInput`, so known heavy commands
   (`make check`, `flutter test`, `go test ./...`, `golangci-lint run`) become
   `saddle run --class C -- <original>`.
   - Rewriting costs no turn and no context. A deny ("run it through
     `saddle run`") costs the agent a round trip every time, and agents
     forget.
   - Compound or odd commands (pipes into heavy tools, `cd x && make check`)
     wrap the whole command line in one lease. The lease is for CPU, so over-
     covering a light prefix is fine.
   - The hook already parses shell (`hook.segments`, used for `--no-verify`
     in #212), so matching against `[runq] classes` patterns is cheap.
3. **(b) PATH shims in agent panes** for `flutter`, `dart`, `golangci-lint`
   and `go`. Saddle prepends `~/.local/state/saddle/shims` to the PATH of the
   panes it spawns.
   - Shims catch what the hook can't see: heavy tools started by the repo's
     git pre-commit hook, by Makefile recipes, and by scripts.
   - Inside a lease (the env token is set) a shim execs the real binary at
     once, so `make check` rewritten by (a) takes one lease and every tool
     under it rides on it.
   - Light subcommands (`go version`, `go build`, `dart --version`,
     `flutter doctor`) match no class pattern and exec straight through for
     about 2ms, without opening the db.
   - Trade-offs: shims shadow the real tools, so they must find the real one
     by skipping their own directory on PATH. Absolute-path calls slip past
     them. `flutter` is a shell script that runs `dart`; the env token keeps
     that from double-queueing.
4. **(c') The repo's own Makefile and hooks:** no. lintgate can *detect*
   heavy targets, but saddle must not edit the owner's Makefile or hooks;
   (b) covers them without that.

Heavy patterns live in config. The repo's `.saddle/config.toml` declares
classes and argv patterns; the slot counts belong to the machine (see Q7):

```toml
[runq.classes.flutter-test]
match = ["flutter test*", "make test-flutter*"]
[runq.classes.go-test]
match = ["go test*", "make check", "make test*"]
[runq.classes.golangci-lint]
match = ["golangci-lint run*", "make check/lint"]
```

### 2. Coordination primitive across sessions without a daemon

| Option | Liveness on crash | FIFO + priorities | Introspection (who and why) | Cross-repo | Verdict |
| --- | --- | --- | --- | --- | --- |
| **SQLite lease table + per-lease flock (chosen)** | Kernel-exact through flock; heartbeat fallback | Yes, in SQL and Go | Full: holders, waiters, history | Yes (user-level file) | **Recommend** |
| flock slot files only (`slot-N.lock`) | Kernel-exact | **No**: who gets a released lock is unspecified, so there are no priorities and no fairness | Lock holders only; waiters can't be seen | Yes | Too weak: no queue position, no priority |
| SQLite only, PID plus heartbeat liveness | PID reuse and `/proc` start-time parsing; otherwise a heartbeat timeout (slow) | Yes | Full | Yes | The flock is cheaper and exact |
| Small local service on a unix socket | Exact (the socket closes) | Yes | Full, push-based (no polling at all) | Yes | Someone has to start and supervise it, and a dead daemon wedges everyone. Revisit only if polling cost ever matters. |
| Per-repo `.saddle/state.db` | Same as SQLite | Yes | Yes | **No**: two repos on one CPU wouldn't see each other | Wrong scope |

Notes:

- SQLite with WAL and `synchronous=NORMAL` is safe against process crashes;
  only power loss can roll back the last transactions. Losing a transaction
  of queue state is harmless.
- Each attempt is one short `BEGIN IMMEDIATE`. Six waiters polling at
  ≤1s is about 6 tiny writes a second.
- Per-class slots, priorities, heartbeat, takeover and fairness are all in
  the prototype. Default priorities: `PrioGate` 20 (train and pre-publish),
  `PrioWorker` 10, `PrioBackground` 0. Aging adds +1 every 30s.
- A holder that re-queues goes to the back, so one agent can't hog a class.

### 3. Load awareness

- **Gate.** `runq.Gate` is an interface, and `LoadGate` reads `/proc/loadavg`
  and `/proc/pressure/cpu`.
  - It holds the head of a class while `load1/ncpu > max_load_per_cpu`
    (default 1.0) or PSI `some avg10 > max_cpu_pressure` (default 60%).
  - The hold shows in the status line ("waiting on load: cpu pressure 72% >
    60%").
  - It fails open on an unreadable probe (macOS, containers without PSI) and
    gives up after `gate_max_wait` (10m), so a box busy for its own reasons
    slows saddle but never stops it.
- **Adaptive slots (later).** Once history has per-class CPU cost (Q9), set
  `slots(class) = max(1, floor(cores × target_util / avg_cores(class)))`. Until
  then the slots are static and the gate does the adapting.
- **Second line of defense.**
  - Recommended: start heavy children under `nice -n 10` and, on Linux,
    `ionice -c3`. They're free, portable, and keep the desktop and the agents'
    own CLIs responsive.
  - Optional: `systemd-run --user --scope -p CPUWeight=20`, where a user
    systemd exists. Prefer `CPUWeight` over `CPUQuota`, because a quota idles
    cores even when nothing else wants them.
  - The ticket's acceptance item "CPU stays under a configurable ceiling"
    maps to the load gate plus slots. A hard ceiling (`CPUQuota=`) is an
    opt-in `[runq] cpu_quota`.

### 4. Is it a CI runner?

1. **(i) Queue and lease wrapper: recommended.**
   - It is small, runs the command where the agent already is (same worktree,
     same toolchain, same caches) and streams its output unchanged.
   - It needs no new trust boundary.
   - Every caller can use it: agents, gates, hooks, other repos, the owner's
     own shell.
2. **(ii) A local job runner** (executes, captures logs, caches by tree hash):
   **later, as features of (i), not a separate system.**
   - Result caching keyed on `git write-tree` plus command plus class is worth
     having: a pre-publish check of a tree the train just tested green should
     be instant. That fits in `saddle run --cache`, reusing the green-tree
     cache the train and pre-publish seed.
   - Captured logs (`~/.local/state/saddle/runs/<id>.log`) can come with it.
   - Neither needs a scheduler of its own.
3. **(iii) A self-hosted GitHub Actions runner: not recommended.**
   - It adds a network round trip and registration-token management.
   - It runs PR code next to the owner's home directory, which is a security
     boundary problem.
   - GitHub's scheduler knows nothing about local priorities.
   - Agents need feedback in seconds, inside their worktree, before anything
     is pushed.
   - If one is ever added, its job steps call `saddle run` and share the
     same queue.

### 5. Agent UX

- **What the agent sees.** One line on stderr when it queues, again only when
  its position or holder changes, and one when it starts:

  ```
  queued: position 2 of 3 for flutter-test, holder t83 (flutter test, 2m10s), ~4m
  runq: flutter-test slot acquired after 3m52s
  ```

  The `~4m` estimate is the class's median `held_ms` from history times the
  position. The agent never polls; the command just takes longer, so waiting
  costs no context.
- **The Bash tool timeout is the real risk.** Claude Code's Bash tool
  defaults to 2 minutes.
  - Saddle should set `BASH_DEFAULT_TIMEOUT_MS` and `BASH_MAX_TIMEOUT_MS`
    (for example 30 and 60 minutes) in the panes it spawns.
  - The queued line should end with "(starts automatically; this command may
    take a while)".
  - Agents' prompts should mention `run_in_background` for long gates.
- **Timeouts.** `saddle run --wait-max 30m` (the default) gives up with exit
  code 75 (EX_TEMPFAIL) and a message naming the holder. The message says to
  report it in `done` or ask the orchestrator, not to retry in a loop.
  - The holder's own runtime has a per-class `max_run` (default 30m). After
    it, the lease shows as overdue in status and the narrator flags it.
  - Saddle does not kill a long run automatically; the owner or orchestrator
    can with `saddle runq kill`.
- **Orchestrator and TUI.**
  - `saddle runq status`, also as a section of `saddle status` and the MCP
    `status` tool, lists per class: holder(s), waiters with their positions,
    wait times and an ETA.
  - The TUI shows a compact segment (`go-test ▸t83 3m · 2 waiting`) and
    a detail pane.
  - The narrator mentions waits over 5 minutes and overdue holders.

### 6. Backpressure

- **Spawn refuses when the queue is backed up**, the same way the bots limit
  (#176) refuses at the cap. The trigger: any class has more than
  `2 × slots` waiters, or its oldest waiter has waited longer than
  `runq.backpressure_wait` (10m). The refusal names the class, the queue
  length and the ETA, so the orchestrator can say why.
- **Gates are exempt.** Landing frees capacity, so it must never be blocked
  by backpressure.
- **Escape hatches.** `saddle spawn --force` (an owner override) and
  `saddle runq drain`. The usage-limit logic (#40) can read the same signal:
  a long heavy-run queue means spending tokens on new workers buys little.

### 7. Cross-repo and multi-machine scope

- **Per user, per machine is enough, and it's what the prototype does.** The
  file sits in `$XDG_STATE_HOME`, so every repo and every session on the
  machine shares it. CPU is a machine resource; two machines don't contend
  for it.
- **Class names are global** (`go-test` means the same thing in any repo).
  - Slot counts are a property of the machine: they go in user config
    (`~/.config/saddle/runq.toml`), and a repo's `[runq]` only supplies
    defaults.
  - The first writer of a class seeds its slots; `saddle runq slots` changes
    them for everyone.
- **A remote box** runs its own queue. Nothing changes, because each machine
  has its own state dir.
- **A shared NFS home** is the one hazard: flock over NFS is unreliable.
  Leases record their `host`, and only same-host leases are flock-probed;
  other hosts fall back to the heartbeat. `saddle doctor` should warn if
  `$XDG_STATE_HOME` is on NFS.
- **Containers** that share a CPU with the host but not its state dir won't
  coordinate. Mount the state dir in if that matters.

### 8. Safety

- **A crashed agent never leaves the queue stuck.**
  - flock liveness reaps the dead on the next attempt by any process, within
    `poll_max` ≤ heartbeat/2 (spike: ~260ms).
  - The heartbeat covers what flock can't see.
  - Pdeathsig kills an orphaned child.
  - Orphan lock files are swept, and a corrupt db is moved aside.
- **No deadlock between a pre-commit hook and a nested `make check`.**
  - The lease token passes through `SADDLE_RUNQ_LEASE`, and nested runs of
    any class ride on it, so a process never holds one lease while waiting
    for another.
  - The only remaining hold-and-wait would be code that takes two leases
    independently. `Run` makes that impossible for children; in-process
    callers must not do it, and the core ticket adds a lint test for it.
  - A token that leaks into a long-lived process (a daemon started inside a
    run) only rides while the outer lease lives. After that it queues
    normally.
- **Owner controls.**
  - `SADDLE_RUNQ=off` (bypass) and `SADDLE_RUNQ=observe` (record only, Q9).
  - `saddle runq status | kill <id> | drain [class] | slots <class> <n>`.
  - Drain sets the slots to 0: running work finishes, nothing new starts,
    and waiters show "drained" in their status line. `slots` undoes it.
  - Every guard has an escape hatch (#119): bypass, kill, `GateMaxWait`,
    `--wait-max`.

### 9. Measure first

- **History.** The prototype's `history` table already records `waited_ms`,
  `held_ms` and how each run ended, per class.
  - The core ticket adds CPU time and peak RSS from
    `cmd.ProcessState.SysUsage()`. That is the child's `rusage` from wait4,
    which includes the descendants it reaped.
  - `cpu_ms / held_ms` gives the average number of cores a class uses.
- **Plan.**
  1. Ship `saddle run`, the gate integration and the interception in
     `SADDLE_RUNQ=observe` mode for a week: it records but never waits.
  2. `saddle runq stats` shows p50/p95 duration, average cores and peak RSS
     per class, and how often runs of each class overlapped.
  3. Set the slots from that data, then switch the default to enforcing.
  - `saddle status` shows a one-line summary.

## Risks and open items

- **Bash tool timeouts (Q5)** are the most likely way the UX breaks. The
  core ticket must set the timeout env vars.
- **Hook rewrite visibility:** the agent sees its command run as
  `saddle run … -- make check`. That's fine and even helpful, but the agent
  prompts should explain it once.
- **Non-Linux:** flock and fsnotify work on macOS; Pdeathsig and PSI don't.
  There, orphaned children survive a killed holder, and the gate uses load
  average only.

## Follow-up tickets

Created from this doc; each links #236 and states its model scope.

1. #238: core `saddle run` and `saddle runq` CLI on top of `internal/runq`, with `[runq]` config, observe mode, rusage history and agent timeout env. **Opus**.
2. #239: route the train gate, pre-publish layer checks and CI-red repairs through the queue at `PrioGate`. **Opus**.
3. #240: PreToolUse rewrite of heavy Bash commands and PATH shims in agent panes. **Opus**.
4. #241: load-aware gating, nice/ionice, an optional systemd scope and adaptive slots. **Opus**.
5. #242: queue in `saddle status`, MCP status, TUI and narrator. **Opus**.
6. #243: spawn refuses while the heavy-run queue is backed up. **Opus**.
7. #244: `saddle runq stats` from history. **Sonnet**.
8. #245: e2e journeys through the real saddle binary (two sessions, crashed holder, hook re-entrancy, 6-agent CPU ceiling). **Opus**.
