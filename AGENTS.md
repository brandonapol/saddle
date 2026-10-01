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

## Testing
- When you find something that does not work right, write a failing test that
  reproduces it first, then fix it. New behavior ships with tests. Name the
  covering tests in your PR description or done summary.
