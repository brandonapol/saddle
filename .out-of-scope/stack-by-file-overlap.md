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
Only a declared `after` stacks. Everything else gets its own PR on main. See
[ADR 0012](../docs/adr/0012-branches-are-the-truth.md).
