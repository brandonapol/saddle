# A daemon (`saddled`) with socket RPC and an event bus

- Rejected in: #5 (closed as not planned). Recorded in ADR 0005.

## The idea
`saddle up` starts a long-lived `saddled`; every command, hook and MCP server
talks to it over JSON-RPC on `.saddle/saddled.sock`, and it publishes events
to the TUI.

## Why not
It is one more process to start, find, upgrade and restart, and its crash
takes every command with it. Hooks and MCP servers are short processes the
agent's CLI starts; making each of them reach a daemon adds a failure mode
for no gain. SQLite in WAL mode already lets many processes share state
safely.

## What saddle does instead
Every process opens `.saddle/state.db` directly. `saddle up` or `saddle
plugin engine` hosts the long-lived loops, one per repo (`.saddle/up.lock`).
See [ADR 0005](../docs/adr/0005-no-daemon.md).
