# Stall budget

One blocker should cost one PR, not the night (#406). Every needs-you
decision card (#404) carries a budget. When the budget runs out and nobody
has answered, saddle runs the card's route-around default and keeps work
moving. It never picks a destructive default.

## Config

```toml
[stall]
budget = "30m"      # how long any card waits for an answer (default 30m)
idle_alarm = "45m"  # the no-progress watchdog's threshold (default 45m)
done_orphan = "20m" # how long a done-orphaned worker sits still per step (default 20m)

[stall.kinds]       # per-kind budgets override budget
stuck-stack = "1h"
ci-repair-cap = "30m"
ask-owner = "2h"
risky-prompt = "20m"
```

A card's timeout is its kind's budget. `StallHolds` lists every open card and
every red or conflicting layer, each with `since` and `budget`. Their age is
measured from `since`, so it updates in place and nothing is re-sent.

## Route-around defaults

The sentinel runs `CheckStuck` every cycle, and `CheckStuck` then runs
`StallTick`. `StallTick` expires due cards through `ExpireDecisions`:

| Card | Default after the budget |
| --- | --- |
| `stuck-stack` | The red layer leaves the stack and waits as a standalone PR against base: its own PR if that already targets base, otherwise a fresh one through `publish`. Layers above it that build on it leave with it and stay held. A layer builds on the red one when it runs after it (`deps`), was spawned by it, or changes a file it changes. Serial files don't count. The other layers restack onto base and keep merging. Once base has the red layer's work, the held layers are requeued. |
| `ci-repair-cap` | Parks the red layer: its PR gets `needs-human` and it stops holding the stack. |
| `risky-prompt` | Parks the worker: its uncommitted work is snapshotted, it is marked paused and its window closes, which frees its slot. `saddle resume <task>` picks it up again. |
| `ask-owner` | Tells the asker to go ahead on its best guess and say so in its done summary. |
| `no-progress` | Nothing. The watchdog posts again only if the cause changes. |

Each route-around leaves one line in chat, for example:

> Routed around t150 after 30m. It's now a standalone PR https://github.com/o/r/pull/366 waiting on you; t151 restacked onto main and keep merging without it.

A default that fails, such as a red layer whose work can't stand alone on
base, leaves its card open, and the next cycle tries again.

## One card per blocker

The stuck-stack alarm keys its cards by stack and reason. It also remembers
which layers it has already reported. When a layer below the blocker merges,
the stack takes the blocker's name. The blocker still gets no second card,
which is what caused the "red for 1h33m" / "red for 31m" re-pings.

## No-progress watchdog

Suppose nothing has landed, merged or started (`landed`, `spawn` or a
merge event) for `idle_alarm`, and open tasks or queued branches exist.
`WatchdogTick` then checks gh auth, adapters and load, and posts one card
naming the single root cause. The causes are checked in this order:

1. `gh-auth`: `gh auth status` fails.
2. `quota`: every usable adapter is out of quota.
3. `prompts`: every live worker is sitting on a prompt.
4. `overload`: the 1-minute load is over twice the CPU count.
5. `unknown`: none of the above.

The watchdog posts nothing more for the same cause and the same stall. When
work moves again, the card closes by itself.

## Done-orphaned workers

A worker can call done, have its gate cancelled, and then go quiet: its
work is committed but its branch is never queued (#467). `StallTick` runs
`OrphanTick` each cycle, which flags a running or idle worker as
`done-orphaned` when all of these hold:

- its last done call's gate was cancelled (`done_called`,
  `done_gate_started`, `done_gate_interrupted` or `done_gate_stale` is its
  last done event; a red gate is the agent's to fix);
- no gate holds its done-job lock, and it is not queued;
- its screen has not changed for `done_orphan`.

A flagged worker gets a `done-orphaned` card, shows in the headline and in
`saddle status` (`done_orphaned`), and is healed one step per `done_orphan`
window, with no human step:

1. Saddle re-runs the done gate itself with the done call's summary, so a
   clean, committed branch queues itself.
2. If that can't run (a dirty tree) or the branch still isn't queued, saddle
   queues a notice and types the wake line, pressing Enter again while the
   text sits in the input box.
3. If the screen still hasn't moved, saddle kills the session and spawns a
   finisher on its branch, with its claims and summary, to check it and
   call done.

Any screen change means the agent is alive: the watch starts over and the
card closes. A done gate the train cancelled by rebasing the branch onto a
newer landing restarts on the rebased tree by itself (up to three times),
so the agent never has to call done again for it.

## Issues

The stuck-stack alarm's `stack_stuck` event is what the blocker filer
(#407) turns into a deduplicated GitHub issue. A blocker that gets routed
around therefore already has its issue. The route-around itself is recorded
as a `stall_routed` event. The blocker filer comments it onto that issue (never a new one).
