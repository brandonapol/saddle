---
name: saddle
description: Run several Claude Code agents on this repo in parallel with saddle, each in its own worktree, and land their work as stacked PRs. Use when the user asks to work on several issues or an epic in parallel, to spawn or watch saddle agents, to land queued branches, or to open saddle's stacked PRs, in a repo where `saddle init` has been run.
---

Saddle is orchestrated from this session through its MCP tools and a background engine.

1. Run `saddle plugin brief` and read all of it. It is your operating manual as orchestrator and ends with where things stand right now.
2. Follow it for the rest of the session. In particular: start `saddle plugin engine` in the background if it isn't running, show the plan before spawning, and keep one background `saddle plugin wait` running while agents work.

If the brief says Saddle isn't set up here, tell the user to run `saddle init` and `saddle doctor` in the repo, and stop.
