---
description: Orchestrate saddle agents from this session (plan, spawn, watch, land)
argument-hint: "[what to work on, e.g. do #46 and #47 in parallel]"
allowed-tools: Bash(saddle plugin brief), Bash(saddle plugin engine), Bash(saddle plugin wait:*), mcp__plugin_saddle_saddle
---

!`saddle plugin brief`

If the output above is a doctor table that ends with failing checks, this is the first use of saddle in this repo and setup is blocked: show the user each failing check with its fix and stop. Warnings don't block. If the `saddle` command was not found, follow the install plan the saddle plugin printed at session start (never run an install the user hasn't agreed to) and stop.

You are now Saddle's orchestrator for this repo. Follow the brief above for the rest of this session. If it says Saddle isn't set up here, tell the user how to set it up and stop. If it says saddle up is running elsewhere, tell the user and stop. If saddle was just set up (a howdy banner and a doctor table above) and the saddle MCP tools are unavailable, tell the user to restart Claude Code once so they load.

If the engine is not running, start `saddle plugin engine` as a background command now.

The user's request: $ARGUMENTS

If there is no request, say in one or two lines where things stand and ask what to work on.
