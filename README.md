# saddle

Saddle runs a group of coding agents from one terminal. You give it an epic.
It plans a task graph and runs each task as a Claude Code (or Codex or Grok)
agent in its own tmux window and git worktree. It keeps the agents from
colliding, lands their work serially as stacked PRs, and uses a cheap
narrator to tell you what every window is doing.

The goal is that you stop paying Opus to rebase.

- Design: [TUI canvas](https://claude.ai/artifact/TKRvnLJoDLBonpdKDU19L4)
- Architecture: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)
- Roadmap: GitHub epics, milestone **M0: dogfood**

## Quickstart

```sh
make setup            # dev tools; asks for a Jev key / Claude login only if missing
make install          # puts saddle on your PATH
cd your-repo
saddle up             # opens the TUI
```

Then talk to the orchestrator in the right-hand chat:

> work on #46 and #47 in parallel

> here's an epic: …, plan it and show me before starting

The orchestrator is a headless Sonnet session. It reads your issues, proposes
a plan with disjoint path claims, and starts Opus workers once you say go.
The workers run Claude Code in a hidden tmux session. You see them as rows in
**AGENTS** and as a live **PEEK** at the selected one's terminal.

You don't babysit them. When an agent hits a prompt, stops without finishing
or conflicts, Saddle hands its screen to the orchestrator. The orchestrator
answers it if that's safe, or tells you in chat what's needed. Finished agents
call `done`. The orchestrator runs the merge train (one branch at a time, so
moved directories are followed and conflicts go back to the agent that wrote
them) and offers stacked PRs that close the issues.

| Key | |
|---|---|
| `enter` / `alt+enter` | send / newline |
| `tab` | switch between chat and the agent list |
| `j` `k` | select an agent (peek follows) |
| `enter` on an agent | open its real terminal; `ctrl-b d` comes back |
| `x` / `L` | kill the agent / land queued branches |
| `ctrl+r` | restart the orchestrator (resumes the conversation) |
| `ctrl+c` | quit. Agents keep running; `saddle up` reconnects |

`saddle down` stops every agent. Worktrees and branches are kept.

Config lives in `.saddle/config.toml` (`saddle init` writes a template):
worker and orchestrator models, concurrency, the test command the merge train
runs, and serial files only the train may touch.

### Under the hood

The TUI uses the same building blocks you can call yourself:
`saddle spawn | status | claim | done | land | sync | prs | message | kill`.

How agents are kept apart:
- Each agent works in its own worktree.
- A Claude Code PreToolUse hook denies writes to files another task has
  claimed, and the denial names the owner. Unclaimed files are claimed on
  first write.
- Notices reach agents through hooks. Inside a worktree, `saddle sync` rebases
  onto everything that has landed.
