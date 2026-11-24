# ADR 0013: Saddle v2 is one engine over SQLite, headless turns, and the forge as the merger

- Status: Proposed
- Issue: #955. Bends ADR 0005. Accepts ADR 0010 in a
  stronger form. Builds on ADR 0012 and removes its `after` stacking.
  Keeps ADR 0001, 0002, 0004 and 0006.

## Context

v1 works but costs too much to keep working. It is about 120,000 lines
outside tests. State lives in the database, in roughly 40 JSON files beside
it, and in status derived by replaying events (#827, #921, #925). Two
processes can host the loops, kept apart by five lock files. Workers are
terminals whose state is guessed from the screen (ADR 0010's context). The
TUI hosts the loops and does I/O every second, so it is slow. Saddle
performs merges itself, so it has to model each repo rule and misses some
(#753).

The full design is `docs/v2/ARCHITECTURE.md`.

## Decision

1. All state is rows in `.saddle/state.db`. No state files.
2. A task has one stored state out of six (`ready`, `working`, `blocked`,
   `pr_open`, `merged`, `closed`) with a reason, changed only by one pure
   transition function.
3. One `saddle engine` per repo observes, plans with a pure function and
   applies idempotent actions. No process talks to it: every command, MCP
   server and the TUI read and write SQLite, and the engine notices.
4. Agents run as headless turns, one process per turn, on a single driver
   contract for claude, codex and grok. A worker ends by calling the
   `finish` or `ask` MCP tool. Saddle reads no screen and runs no hooks.
5. The orchestrator is an agent like any other and is disposable: every
   turn starts from a digest of the database.
6. Every branch is cut from main and every PR targets main. `after` means
   "start once that task has merged".
7. The forge merges. Saddle opens the PR, validates it against the repo's
   rules, and asks for auto-merge or the merge queue.
8. Path claims become `Touches`, used only by the scheduler.
9. The TUI is a reader of the database and the logs, with no I/O in its
   update loop.

## Alternatives considered

- **Keep refactoring v1.** ADR 0012 and #827 do this. They remove the worst
  of the PR graph but leave the state files, both loop hosts, tmux and the
  TUI's I/O.
- **No engine: let the TUI or each command host the loops (ADR 0005).** That
  is v1. The loops stop when the TUI closes and the TUI can't be fast.
- **A daemon with socket RPC (`.out-of-scope/daemon.md`).** Still rejected.
  The engine has no RPC, and nothing fails when it is down.
- **Interactive agents in PTYs with screen manifests, as herdr does.** It
  suits a human driving many agents. Unattended, a turn with a typed result
  needs no detection at all.
- **Keep stacked PRs for `after`.** It keeps retargets and `rebase --onto`,
  which is where git and the forge drift apart. Waiting costs one CI run.
- **Rewrite in Rust for the TUI.** The slowness is I/O and re-rendering in
  the update loop, not Go.

## Consequences

- Dependent tasks run one after another, not stacked.
- Two tasks can edit the same file if neither declared it; the newer PR's
  agent fixes the conflict.
- A human can't type into a running worker. They message it or `attach`.
- Narrator, forecast, sentinel, planner view, runq, SRE mode, remote
  control, SSO, GitLab, and the gemini and opencode adapters are not in 2.0.
- v1 keeps shipping from the repo root until v2, in `v2/`, can build itself.
