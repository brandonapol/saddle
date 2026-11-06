# A single-goroutine reactor making every scheduling decision

- Rejected in: #45 (closed as not planned, descoped with #5). Recorded in
  `docs/backlog-audit.md` and ADR 0005.

## The idea
One event loop receives every input (hook events, git deltas, MCP calls,
train transitions, timers) and makes every scheduling decision, so decisions
are totally ordered and replayable.

## Why not
It needs one process that sees every input, which is the daemon of #5.
Without a daemon, inputs arrive in many short processes.

## What saddle does instead
Decisions are made inline in `internal/app`, coordinated through SQLite
transactions, and logged to the `events` table with a reason so they can be
explained afterwards.
