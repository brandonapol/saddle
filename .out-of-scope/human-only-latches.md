# Guards that only a human can clear

- Rejected in: #119 (tolerate humans using GitHub; every guard needs a
  self-service escape). Restated in `AGENTS.md` ("Getting unstuck") and
  ADR 0004 ("no failure latches").

## The idea
When saddle sees something it doesn't understand (a flagged stack, a red
layer, a failed default), stop and wait for the owner to clear a flag.

## Why not
Saddle runs overnight. A latch that only a human clears stops every layer
behind it until morning, and the audit of 2026-11-05 found holds that lasted
days (#774, #776). Most of those states also fix themselves once saddle
looks at GitHub again.

## What saddle does instead
Every guard, hold, sentinel or freeze lifts on its own when the condition
clears, times out into a decision card with a default, or offers a command
the orchestrator can run. Saddle reconciles its state against GitHub rather
than trusting a flag (ADR 0004).
