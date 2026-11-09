# ADR 0011: A batch is one linear stack, merged once by fast-forward

- Status: Superseded by [ADR 0012](0012-branches-are-the-truth.md)
- Issue: #913. Related: #912, #908, ADR 0003, ADR 0007, ADR 0008, ADR 0009,
  #832, #833, #836

## Context

In fbx67 (Nov 8-9 2026) saddle landed about 20 tasks overnight and merged
none between 01:00 and 05:00. Thirteen PRs stayed open, eleven of them in one
chain on other tasks' branches, each showing a red `saddle/base`. The events
show why, and it is the layout, not an edge case:

- Workers build on `saddle/integration`, which holds every landed but
  unmerged task, so their work can depend on siblings that haven't merged
  (t42 imports `ambient.ts` from t38). PRs are cut against `main`.
- `prLayout` unions every task linked by an `after`, a shared issue, task
  text naming another task, or a replay conflict, then stacks each group as
  one chain in train order. A dependency graph becomes a line: t34 sat on the
  unrelated t35 because "t33 runs after t34", and t22 moved onto t26 because
  of t31's dependency. A replay conflict falls back to "the latest layer",
  which is stacking by file overlap (rejected in ADR 0007).
- Auto-merge squashes the bottom PR, then restacks and force-pushes every
  layer above, which restarts CI on all of them. 439 rebases; one merge per
  CI cycle at best, and none while the train lock was busy.
- A red layer held every layer above it, froze its files for landing, and
  got repairs that branched from integration, where the failure doesn't
  reproduce: seven repairs ended "no change" or added a layer (#912).

The owner wants, for repos like fbx67, a stack of per-task PRs that keeps
PR and commit history, merged once rather than layer by layer.

## Decision

1. `[train] output = "batch"` lays every landed task out as one linear
   stack in train order. Each layer's PR is published at the commit the
   train landed: no replay, no clustering, no conflict-driven stacking. The
   train tested each of those commits, so the pre-publish gate's tree cache
   already has them.
2. Auto-merge merges a batch once (`app.BatchMerge`): it reads each layer's
   PR bottom up, stops below the first one that is closed, a draft, labeled
   needs-human, conflicting or held, and takes the highest layer below that
   whose CI is green. It points every PR up to that layer at base, then
   pushes that layer's commit to base as a fast-forward. Nothing is
   rewritten, so the forge marks each of those PRs merged by itself, and
   main keeps every task's commits. The tasks become *merged*, and the
   restack only retargets the layers above to base.
3. If base has commits the batch lacks (a hand merge), the batch merge
   restacks the batch onto base first and merges on a later tick.
4. In a batch, red CI on a layer holds nothing above it, freezes no files,
   and doesn't block auto-merge of a green layer above it. A red layer gets a
   repair only when no layer above it is green or running: a green one
   records `carried` and the merge carries it, and a running one leaves the
   repair pending until its CI finishes.
5. A stacked batch layer's `saddle/base` status is green ("merges with the
   green layer above it"), since it never merges by its own button.
6. alt+b in the TUI flips `train.output` between `batch` and `per-task`
   (one PR per task on base, stacked only where its commits need another's).
   It is saved in `.saddle/config.toml`. Turning batch on also turns
   auto-merge on, as infinite mode does.

This bends ADR 0007 by the owner's choice per repo: ADR 0007 still governs
`stack` and `per-task`.

## Alternatives considered

- **Squash-merge the top PR (collapse, #196).** One commit per batch on
  main; loses per-task commits and PR merge status. The owner chose to keep
  history.
- **Rebase-merge the top PR through the forge.** Keeps per-task commits but
  rewrites them, so lower PRs must be closed by hand and upper layers
  restacked.
- **Fix the clustering in `prLayout`.** The dependency-graph engine
  (`internal/prgraph`, ADR 0009) already models stacks as a DAG of explicit
  edges, and its shadow log in fbx67 planned the flat layout and the merges
  that legacy didn't make. Patching the legacy layout is work #832 deletes.

## Consequences

- One CI run per layer, ever: merges don't rewrite layers, so they don't
  restart CI. A red layer costs nothing until it is the top of the batch.
- main can carry a commit whose own CI was red, when a later layer of the
  same merge fixed it. Bisecting may land on one.
- The batch merge pushes to base directly. A ruleset that forbids direct
  pushes (or requires reviews) makes it fail with the push error and
  nothing merged; a forge-merge fallback is follow-up work.
- The batch grows while nothing above its bottom is green. Autopilot doesn't
  yet cap how many unmerged layers it opens (#913 follow-up).
- prgraph's cutovers (#832 publishing, #833 merging) must carry batch mode:
  a batch is a change graph where each change's only edge is the change
  before it in train order, and its merge action is a fast-forward to the
  highest green change.
