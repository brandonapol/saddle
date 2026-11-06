# Architecture decision records

An ADR records one decision saddle has made, why, and what it costs. Read the
ones that touch your change before proposing a design: if your idea reverses
an accepted ADR, say so in the ticket and argue against the ADR's reasons,
don't just propose the alternative again. Ideas that were rejected outright
are in [.out-of-scope/](../../.out-of-scope/README.md). The words used here are
defined in [GLOSSARY.md](../GLOSSARY.md).

## Index

| ADR | Decision | Status |
|---|---|---|
| [0001](0001-forge-layer.md) | One forge interface over `gh` and `glab` | Accepted |
| [0002](0002-orchestrator-on-any-agent.md) | The orchestrator runs on codex or grok, not only Claude Code | Accepted |
| [0003](0003-landed-means-on-main.md) | Landing opens the PR; only merged into base is done | Accepted |
| [0004](0004-reconcile-against-github.md) | Reconcile saddle's state against GitHub; no failure latches | Proposed |
| [0005](0005-no-daemon.md) | No daemon: saddle runs in-process on SQLite | Accepted |
| [0006](0006-never-the-expensive-model-as-synchronizer.md) | Never use the expensive model to synchronize | Accepted |
| [0007](0007-stacked-prs-by-dependency.md) | Stacked PRs by default, stacked only by dependency | Accepted |
| [0008](0008-serial-merge-train.md) | One serial merge train lands all work | Accepted |
| [0009](0009-prgraph-engine.md) | One pure PR-graph engine owns the stack: one schema, one state machine, one Reconcile | Proposed |
| [0010](0010-headless-workers.md) | Unattended workers run headless, one invocation per attempt, with a typed result | Proposed |

## When a change needs an ADR

Write one, and get it accepted, before the code when a change:

- reverses or bends an accepted ADR;
- changes how work moves from a worker to base (the train, the stack, PRs,
  merging), how workers run, or what saddle stores and where;
- spans several packages or adds a new long-lived process or loop;
- or its ticket says "needs an ADR".

Open work that needed one, and its ADR:

- **#779** (pure PR-graph engine): [ADR 0009](0009-prgraph-engine.md). It
  replaces the landed list and the integration branch as the source of
  truth, builds on ADR 0003 and ADR 0004, and uses the glossary's *stack*,
  *layer*, *landed*, *merged*, *held* and *repair*.
- **#804** (headless worker mode): [ADR 0010](0010-headless-workers.md). It
  changes how workers run and how saddle learns their state, and keeps tmux
  for attended use.

## Conventions

- File name: `NNNN-short-slug.md`, numbered in order, never reused. Take the
  next free number when you open the PR; if two PRs race, the later one
  renumbers.
- Status is one of **Proposed** (under review, not yet the rule), **Accepted**,
  **Superseded by ADR NNNN** or **Rejected**. Never delete an ADR; supersede it
  and link both ways.
- Keep it short: a reader should finish in five minutes. Long evidence goes in
  the issue; the ADR links it.
- Use glossary words. A new term goes into `docs/GLOSSARY.md` in the same PR.
- An accepted ADR changes only to fix facts or add a "Superseded by" line.
  Changing the decision means a new ADR.
- Add the ADR to the index above.

## Template

```markdown
# ADR NNNN: <the decision, as a sentence>

- Status: Proposed
- Issue: #<the issue it decides>. Related: #…

## Context

What forces the decision: the problem, the evidence (issues, incidents,
numbers), and the constraints.

## Decision

What saddle does, stated as rules a reviewer can check code against.

## Alternatives considered

Each option not taken, and why not. Rejected ideas also get a file in
`.out-of-scope/`.

## Consequences

What gets easier, what gets harder, what follow-up work it creates.
```
