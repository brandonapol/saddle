# An integration branch, and stacks inferred from it

- Rejected in: #930, ADR 0012 (supersedes ADR 0003, ADR 0007, ADR 0008 and
  ADR 0011)

## The idea
Land every finished task, one at a time, onto a local `saddle/integration`
branch, so each is tested on top of everything before it. Then cut that
history back into one PR per task, stacking a PR on another task's PR when
the task seems to depend on it (spawn parent, shared issue, a task naming
another, commits that don't apply without it, a red pre-publish gate), or
merge the whole chain at once (batch mode).

## Why not
The order tasks finish in isn't the order they depend on each other, so
cutting that line back into a graph is guesswork. In fbx67 (2026-11-09) it
produced a 12-deep chain where three PRs didn't contain the PR they were
stacked on, a one-file doc change shipping 8 commits from other tasks at
layer 12, a red `saddle/base` check on every stacked PR, and months of
patches (#751, #836, #912, #913, #915, #916, #919, #920, #922) that never
stopped the drift.

## What saddle does instead
Each task branches off main (or a declared `after` task's branch), opens
one PR, fixes its conflicts and red CI on that same branch, and a plain
queue merges green PRs into main one at a time. See
[ADR 0012](../docs/adr/0012-branches-are-the-truth.md).
