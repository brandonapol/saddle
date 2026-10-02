# Agent notes for saddle

## Commits and PRs
- Do not add Claude (or any AI) as a co-author. No `Co-Authored-By: Claude …`
  trailer, no `Claude-Session:` trailer. Commit messages are plain.
  `.claude/settings.json` sets `attribution.commit` to `""` to enforce this.

## Workflow
- `make check` runs vet, tests and golangci-lint, the same checks CI runs. It
  must pass before you push.
- `make install` rebuilds `~/go/bin/saddle`, which spawned agents run. Run `make` alone to list every target.
- Branch off `main`. Don't commit to it directly.

## Layout
See `docs/ARCHITECTURE.md`. Core logic lives in `internal/app`. The CLI
(`internal/cli`), the hook (`internal/hook`) and the MCP server
(`internal/mcpserver`) are thin layers over it.

## Getting unstuck
Saddle's top goal is to get work done, not to gum up the system and need
constant human intervention.
- Agents and the orchestrator may hand-fix things so work keeps moving: edit
  `.saddle/state.db` (back it up first), recreate branches, spawn a worker to
  do a repair, re-land work as fresh PRs. This includes overriding "don't use
  git or a worker" style rules when a tool is stuck and the owner has not
  forbidden it.
- Every hand fix must be followed up in the same session by (1) a regression
  test that reproduces the failure, written failing-first, and (2) a GitHub
  issue designing a better system so the hand fix is never needed again.
  Record the exact fix in the issue.
- Never leave the tool in a state where only manual intervention can unblock
  it. Any guard, sentinel or freeze that blocks work must offer a self-service
  escape hatch (see #119).
- Tell the user what you did in one or two sentences.

## Testing
- When you find something that does not work right, write a failing test that
  reproduces it first, then fix it. New behavior ships with tests. Name the
  covering tests in your PR description or done summary.
