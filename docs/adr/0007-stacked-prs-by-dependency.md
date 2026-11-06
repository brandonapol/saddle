# ADR 0007: Stacked PRs by default, stacked only by dependency

- Status: Accepted (backfilled 2026-11-06)
- Issue: #52 (stacked PRs by default). Refined by #193, #358 (dependencies
  stack whatever the files), #226 and #751 (file overlap does not stack).
  Related: #779, #805

## Context

Saddle lands many tasks a day. Opening one big PR per batch makes review
impossible; opening every PR on base breaks the ones that need a sibling's
change (#193). The first stacked design chained every landed task in train
order, and later linked tasks that touched the same files. Overlap is
transitive, so it chained unrelated work into one stack: 16 PRs that should
have been about 10 stacks (#226), then a 17-deep stack where one merge into a
non-main base lost work and closed about 25 PRs (#751).

## Decision

1. Each landed task gets its own PR. Output is *stacked* PRs: a PR's base is
   the PR of a task it depends on, and a task with no dependencies is a stack
   of one with its PR on base.
2. A dependency is: `after` (on spawn or later), a spawn parent, the task's
   title, prompt or issue naming an earlier task, a PR someone retargeted by
   hand, or a layer the pre-publish gate shows it needs. Sharing a file is
   **not** a dependency.
3. Each stack is restacked when a lower layer changes and retargeted when a
   lower layer merges. On GitHub stacks are also linked as native stacked PRs
   (gh-stack) when available.
4. Layers relate only within their stack. Train order never implies "above"
   (#771).

## Alternatives considered

- **One PR per batch, or one long chain in train order.** Rejected by #751.
- **Stack by file overlap.** Rejected by #226 and #751: transitive overlap
  couples unrelated work.
- **Every PR on base.** Rejected by #193: dependent work fails CI alone.

## Consequences

- Most PRs target base directly and merge independently.
- Missing a dependency shows up as a red pre-publish gate, which names the
  probable `after` edge; saddle does not add it by itself.
- #779 makes the dependency graph, not the landed list, the source of truth.
