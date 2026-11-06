# ADR 0005: No daemon; saddle runs in-process on SQLite

- Status: Accepted (backfilled 2026-11-06 from the decision made on #5)
- Issue: #5 (the `saddled` daemon, closed as not planned). Related: #45 (the
  reactor, descoped with it), #602, #665, #805

## Context

The first design (#1, #5) had a daemon, `saddled`: `saddle up` would start it,
every command would talk to it over JSON-RPC on `.saddle/saddled.sock`, and a
single-goroutine reactor (#45) inside it would make every scheduling decision.

That design adds a process that must be started, found, upgraded and
restarted, and whose crash takes every command with it. Agents already
survive in tmux without saddle, and hooks and the MCP server run as short
processes started by the agent's CLI, where reaching a daemon is one more
thing to fail.

## Decision

1. There is no `saddled`. Every `saddle` command, hook and MCP server opens
   `.saddle/state.db` (SQLite, WAL, busy timeout) and works on it directly.
   Concurrent processes coordinate through SQLite transactions and file locks,
   not a socket.
2. Scheduling decisions are made inline in `internal/app`, not by a central
   reactor (#45 is descoped).
3. Long-lived loops (stack sentinel, CI watcher, autopilot, auto-merge) are
   hosted by whichever front end is running: `saddle up` or `saddle plugin
   engine`. One host per repo, enforced by the flock on `.saddle/up.lock`
   (#665); a crashed host never wedges the next one.
4. Nothing may assume a loop is running. A command that needs a loop's work
   done (land, publish, restack) does it itself.

## Alternatives considered

- **A daemon with socket RPC and an event bus (#5).** Rejected for the reasons
  above. See `.out-of-scope/daemon.md`.
- **A reactor without a daemon.** A single decision goroutine only works if
  one process sees every input; with many short processes it doesn't exist.
  Descoped with #45.

## Consequences

- Any process can crash without losing state; restart reads the database.
- Watchers stop when `saddle up` exits. Work still lands because land and
  done do their own publishing (ADR 0003), but nobody notices a broken stack
  until a host runs again.
- Every new loop must be safe when two processes run it at once: take a
  lock, or give each run its own scratch state, as the PR layout does with
  one worktree per run (#680).
