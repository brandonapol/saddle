---
description: Show saddle's agents, claims and merge train
allowed-tools: Bash(saddle plugin setup), Bash(saddle status), mcp__plugin_saddle_saddle__status
---

!`saddle plugin setup`

If the output above asks "Do you trust" this folder, saddle isn't trusted here yet and has written nothing: show the user that prompt as it is, with both options, and stop until they answer. Only if they choose 1, run `saddle trust --yes` and then run `saddle plugin setup` again; if they choose 2, tell them saddle won't run here and stop. Never answer it for them.

If the output above ends with failing doctor checks, show the user each one with its fix and stop; warnings don't block. If the `saddle` command was not found, follow the install plan the saddle plugin printed at session start (never run an install the user hasn't agreed to) and stop.

Call the saddle `status` tool (if it isn't available because saddle was just set up in this session, run `saddle status` instead) and summarize it in a few lines: each agent by id and title with its status, what is queued in the merge train, what has landed with its PR open (each task's `delivery`), what has merged into base, and any warnings. Work counts as done only when its delivery says merged; never call landed-but-unmerged work done. Lead with anything that needs the user.
