# An LLM merging, rebasing or resolving conflicts

- Rejected in: the design rule in `docs/ARCHITECTURE.md` ("never use the
  expensive model as the synchronizer"), recorded as ADR 0006; restated in
  #805 because it keeps being proposed.

## The idea
Let a strong model (the orchestrator, or Opus in a worker) merge finished
branches, rebase the stack, or resolve conflicts the train hits.

## Why not
It spends the quota saddle exists to put into code, is slow, and gives a
different answer each run, so a failure can't be replayed or covered by a
test. A model that merges also has to understand both sides of every
conflict, which the agent that wrote one side already does.

## What saddle does instead
The serial merge train, rename map, structural merge and regenerated derived
files, then the producing agent, then the owner (ADR 0008). Repairs are
worker tasks that write code on their own branch and land through the train
like any other task. See
[ADR 0006](../docs/adr/0006-never-the-expensive-model-as-synchronizer.md).
