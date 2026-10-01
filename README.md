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

## Quickstart (M0 dogfood)

```sh
go install ./cmd/saddle            # puts saddle on your PATH
cd your-repo && saddle init        # .saddle/config.toml: test cmd, serial files, models
saddle up epics/my-epic.md         # tmux session; window 0 is the orchestrator (Claude)
```

The orchestrator plans the epic and calls the saddle MCP tools: `spawn` puts
each task in its own worktree, branch and tmux window. Agents call `done`.
`land` merges them one at a time onto `saddle/integration`, following
directory moves and sending any conflict back to the agent that produced it.
`prs` opens stacked PRs.

You can also drive it by hand:

```sh
saddle spawn "meter worker" -c 'pkg/meter/**' -m opus -f prompt.md
saddle status                      # tasks, claims, train
saddle message t2 "use the new interface in pkg/meter"
saddle land && saddle prs
```

Inside a task worktree, `saddle sync` rebases onto everything that has landed.

How agents are kept apart:
- Each agent works in its own worktree.
- A Claude Code PreToolUse hook denies writes to files another task has
  claimed, and the denial names the owner. Unclaimed files are claimed on
  first write.
- Notices arrive through hooks. An idle agent is woken by typing into its
  tmux window.
