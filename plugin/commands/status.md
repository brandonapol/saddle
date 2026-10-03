---
description: Show saddle's agents, claims and merge train
allowed-tools: Bash(saddle plugin setup), Bash(saddle status), mcp__plugin_saddle_saddle__status
---

!`saddle plugin setup`

If the output above ends with failing doctor checks, show the user each one with its fix and stop; warnings don't block. If the `saddle` command was not found, follow the install plan the saddle plugin printed at session start (never run an install the user hasn't agreed to) and stop.

Call the saddle `status` tool (if it isn't available because saddle was just set up in this session, run `saddle status` instead) and summarize it in a few lines: each agent by id and title with its status, what is queued or landed in the merge train, and any warnings. Lead with anything that needs the user.
