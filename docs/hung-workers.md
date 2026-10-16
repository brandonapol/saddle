# Hung workers

- Status: Proposed
- Issue: #625; builds on #432 (hung gates in the watchdog), #467 (done-orphaned workers), #481 (mid-task handoff); related #229, #568, #601

## Context

In one session the orchestrator found five workers hung, by hand:

- t298, t299 (grok): silent after a context compaction, 14 to 16 unread
  notices, the screen unchanged for hours.
- t309 (claude): its done gate shown as running for about five hours,
  never queued.
- t318 (grok): compacted, then idle.
- t318, t320 (grok): "Waiting for response" for 20 to 38 minutes.

Each time the recipe was the same: notice it, kill the task, spawn a
takeover that cherry-picks the committed work. Saddle already handles two
neighbours of this. The no-progress watchdog (#406, #432) names a gate
running far past its timeout as the root cause of a stall, but does nothing
about it. The done-orphan watch (#467) heals a worker whose done call was
cancelled. Nothing watched a worker that is mid-task and stops moving.

## Decision

`HungTick` runs every sentinel cycle from `StallTick`, next to
`OrphanTick`. Its state lives in `.saddle/hung-workers.json`.

### What counts as hung

A live worker in a window saddle runs is watched when either holds:

- it is **running** (a turn is in progress, as far as saddle knows), or
- it is **idle with unread notices**: notices queued for it that no hook or
  wake has delivered.

It is flagged hung when its **progress fingerprint** has not changed for
`[stall] hung_after` (default 20m). The fingerprint is a hash of:

- its screen, reduced to letters and spaces, so a spinner, an elapsed
  timer ("Waiting for response 38m 12s") or a token counter is not progress
  but any new line of output is;
- its branch head and `git status`, so a worker that commits or edits files
  is progressing whatever its screen shows.

A gate or test process (`go test`, a test binary, `make check`) running in
its worktree within twice its own timeout is progress too: a slow
`make check` is not a hang. One past twice its timeout is a **hung gate**
(the #432 rule) and is the first thing recovered.

Left alone, so no two watches act on one task:

- a worker queued or on hold in the train;
- a worker the done-orphan watch is watching. #467 owns "done called but
  never queued", including a done gate shown as running whose job lock no
  process holds (t309);
- needs-you, paused, done, conflict and dead tasks, and windows saddle
  doesn't own.

### Recovery ladder

One step per `hung_after` window. Any change of the fingerprint at any step
means the worker is alive: the watch starts over and its card closes.

0. **Hung gate**: when the stillness comes with an overdue gate process in
   its worktree, saddle sends that process SIGTERM first. Nothing committed
   is touched; the gate fails and the agent gets its turn back.
1. **Nudge**: an action notice saying saddle thinks it is stuck, typed into
   the pane with the wake line, its submit checked as #467 does. A
   permission prompt is never typed over: the worker is marked needs-you
   instead.
2. **Enter**: one Enter key, for a TUI sitting on a drafted or half-sent
   line, or on a compaction summary waiting to be dismissed.
3. **Takeover**: the session is replaced in place. The uncommitted work is
   snapshotted to `refs/saddle/wip/<task>`, the window is killed, and a new
   agent starts **in the same worktree on the same branch, with the same
   task id and claims**, told it is taking over. It runs on a hooked
   adapter with quota, preferring one other than the adapter that hung;
   with none, on the same adapter in a fresh session.

The issue asked for "kill and spawn a takeover task seeded with the
branch's commits". Relaunching in place does that and more: it starts from
every commit **and** uncommitted change, keeps the claims and the stack
position, and adds no task to land. It reuses the path the #481 handoff
takes for a worker out of quota.

### Safety

- **Never discards work.** Nothing is reset, removed or force-pushed. The
  worktree and branch stay as they are; the WIP snapshot is a second copy.
- **Respects the cap.** A takeover replaces one session with one session in
  the same task, so it never runs more workers than before.
- **Respects "do not kill".** Each flagged worker gets a `worker-hung` card
  whose second option is "Don't kill it". Answering it, or
  `saddle keep <task>`, records a `hung_keep` event: saddle still nudges,
  but never replaces that task's session. A task whose prompt says "do not
  kill" or "don't kill" is kept the same way.
- **Gives up instead of looping.** After two takeovers of one task saddle
  stops and tells the orchestrator, naming the branch.
- `hung_after = "-1s"` (any negative duration) turns the watch off.

```toml
[stall]
hung_after = "20m" # how long a worker may sit without progress per step
```

## Consequences

- A hung worker costs about three `hung_after` windows (an hour by default)
  instead of hours of the orchestrator's attention.
- A worker in a long, silent thinking turn with nothing on screen moving
  could be nudged. A nudge is a notice and a typed line that a live agent
  reads and moves past; that is the price of catching the real hangs.
- Tests: `TestHungWorkerIsNudgedThenTakenOver` (a worker that stops reading
  notices is detected and replaced), `TestSlowProgressingWorkerIsNotHung`,
  `TestHungTimerOnlyScreenIsStill`, `TestHungKeepNeverKills`,
  `TestHungGateIsKilledFirst`, `TestHungSkipsDoneOrphans`.
