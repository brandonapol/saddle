---
name: saddle
description: Run several Claude Code agents on this repo in parallel with saddle, each in its own worktree, and land their work as stacked PRs. Use when the user asks to work on several issues or an epic in parallel, to spawn or watch saddle agents, to land queued branches, or to open saddle's stacked PRs. On first use in a repo it runs `saddle init` and `saddle doctor` itself.
---

Saddle is orchestrated from this session through its MCP tools and a background engine.

1. Run `saddle plugin brief` and read all of it. It is your operating manual as orchestrator and ends with where things stand right now.
2. Follow it for the rest of the session. In particular: start `saddle plugin engine` in the background if it isn't running, show the plan before spawning, and keep one background `saddle plugin wait` running while agents work.

On first use in a repo, `saddle plugin brief` runs `saddle init` and `saddle doctor` and prints the doctor table. If a check fails, show the user each failing check with its fix and stop; warnings don't block. If the brief says Saddle isn't set up here, tell the user why and stop.
