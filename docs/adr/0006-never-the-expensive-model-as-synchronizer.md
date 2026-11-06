# ADR 0006: Never use the expensive model to synchronize

- Status: Accepted (backfilled 2026-11-06 from the design rule in
  `docs/ARCHITECTURE.md`)
- Issue: #1 (foundation epic), #25 (serial train), #26 (rename map). Related:
  #779, #805

## Context

Running several agents on one repo needs a lot of coordination: who may write
which file, which branch lands first, how conflicts are resolved, how PRs
stack and restack. It is tempting to hand that to a strong model ("let Opus
merge the branches", "let the orchestrator rebase the stack"). That is slow,
expensive on the very quota saddle exists to spend on code, not repeatable,
and it fails in ways nobody can replay or test.

## Decision

The models write code. Synchronization is deterministic code:

1. Isolation (one worktree and branch per task), claims enforced by a hook,
   and a git-aware scheduler keep agents apart.
2. The serial merge train (ADR 0008), the rename map, a structural merge
   driver and regenerating derived files resolve integration. A conflict they
   can't resolve goes back to the **producing agent**, then to the owner.
   No step uses the planner model by default.
3. PR layout, restack, retargeting, auto-merge and repair spawning are code,
   driven by state saddle can test.
4. A cheap model may advise only where rules run out (the Sonnet-class
   dispatcher for gray-zone forecasts) and its answer is a structured choice
   that code carries out (`hold | proceed | sequence | ask_owner | escalate`).
5. The orchestrator steers with tools (`land`, `queue_move`, `restack`,
   `unstack`), never by running git or editing `state.db` itself, except for
   the logged hand fixes `AGENTS.md` allows.

## Alternatives considered

- **An LLM merges or rebases branches.** Rejected; see
  `.out-of-scope/llm-merging.md`.
- **The orchestrator resolves conflicts directly.** It would block the
  session that answers every other agent, and its fixes don't come with tests.

## Consequences

- The TUI counts tokens spent on merges, and the goal is zero.
- Every class of sync failure needs code and a regression test, which is
  slower to build but stays fixed.
- When code can't decide, the answer is a decision card for the owner or a
  repair task (a worker writing code), not a smarter synchronizer.
