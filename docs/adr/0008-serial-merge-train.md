# ADR 0008: One serial merge train lands all work

- Status: Superseded by [ADR 0012](0012-branches-are-the-truth.md)
- Issue: #25 (serial train: queue, rebase, test gate, fast-forward). Related:
  #26 (rename map), #819 and ADR 0003, #836, #779, #805

## Context

Agents finish in any order and their branches overlap. Letting each one push
or merge on its own makes conflicts show up late, in whichever branch merges
second, with nobody who knows both sides. Lockfiles and migrations conflict
on almost every pair of branches.

## Decision

1. Finished branches (`done`) queue in one merge train per repo and land
   **one at a time**: rebase onto the work landed before them, run the test
   gate, fast-forward the train's local base. Then the task's PR opens
   (ADR 0003).
2. A conflict or red gate goes back to the agent that wrote the branch
   (`conflict`, `test_failed`), with the rename map, structural merge and
   regenerated derived files tried first, and to the owner after repeated
   failures (`escalated`). See ADR 0006.
3. *Serial files* (lockfiles, migrations, `go.mod`/`go.sum` here) belong to
   the train; agents can't claim them.
4. Only the train moves the integration branch and only it publishes. Agents
   never push, merge or rebase onto other branches; they `saddle sync`.
5. The owner steers the queue with `saddle queue hold|move|release`, not by
   editing it.

## Alternatives considered

- **Agents merge their own PRs as they finish.** Conflicts land on whoever is
  second and serial files break constantly.
- **Parallel landing with optimistic retries.** More throughput, but a
  conflict surfaces in a branch that didn't cause it.
- **An LLM resolves the conflicts.** See ADR 0006.

## Consequences

- Landing is a bottleneck by design; the test gate's speed sets throughput.
- Every conflict has exactly one owner.
- The train's local base is an internal detail, not a destination (ADR 0003);
  #836 and #779 plan to cut each change from base plus its dependencies
  instead. Landing stays serial either way.
