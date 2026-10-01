# Agent notes for saddle

## Commits and PRs
- Do not add Claude (or any AI) as a co-author. No `Co-Authored-By: Claude …`
  trailer, no `Claude-Session:` trailer. Commit messages are plain.
  `.claude/settings.json` sets `attribution.commit` to `""` to enforce this.

## Workflow
- `make check` runs vet, tests and golangci-lint, the same checks CI runs. It
  must pass before you push.
- `make install` rebuilds `~/go/bin/saddle`, which spawned agents run.
- Branch off `main`. Don't commit to it directly.

## Layout
See `docs/ARCHITECTURE.md`. Core logic lives in `internal/app`. The CLI
(`internal/cli`), the hook (`internal/hook`) and the MCP server
(`internal/mcpserver`) are thin layers over it.
