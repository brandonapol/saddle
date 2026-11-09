# Out of scope

One file per idea saddle has considered and decided against, with the reason
and the issue where it was decided. Before proposing a feature, check here
and in [docs/adr/](../docs/adr/README.md). If you still think the idea is
right, open an issue that answers the recorded reason; don't just build it.

| Idea | Decided in |
|---|---|
| [A daemon (`saddled`) with socket RPC](daemon.md) | #5, ADR 0005 |
| [A single-goroutine reactor making every decision](reactor.md) | #45 |
| [An LLM merging, rebasing or resolving conflicts](llm-merging.md) | ADR 0006, #805 |
| [Stacking tasks because they touch the same files](stack-by-file-overlap.md) | #226, #751, ADR 0007 |
| [Shipping release binaries](release-binaries.md) | #544 |
| [Guards that only a human can clear](human-only-latches.md) | #119, ADR 0004 |
| [An integration branch, and stacks inferred from it](integration-branch-and-inferred-stacks.md) | #930, ADR 0012 |

## Format

```markdown
# <the idea>

- Rejected in: #<issue> (and ADR NNNN if one records it)

## The idea
One paragraph, the strongest version of it.

## Why not
The reason, with evidence.

## What saddle does instead
Pointer to the design that covers the need.
```
