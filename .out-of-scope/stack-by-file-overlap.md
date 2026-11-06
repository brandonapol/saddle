# Stacking tasks because they touch the same files

- Rejected in: #226 and #751. Recorded in ADR 0007.

## The idea
If two landed tasks change the same files, base the later one's PR on the
earlier one's, so it reviews and merges in the right order.

## Why not
Overlap is transitive: A shares a file with B, B with C, so A and C end up in
one stack though they are unrelated. In practice it produced 16 PRs that
should have been about 10 stacks (#226), then a 17-deep stack in which one
merge into a non-main base lost work and closed about 25 PRs (#751).

## What saddle does instead
Only dependencies stack: `after`, a spawn parent, a named earlier task, a
hand retarget, or what the pre-publish gate shows a layer needs. Everything
else gets its own PR on base. See
[ADR 0007](../docs/adr/0007-stacked-prs-by-dependency.md).
