# Previewing and publishing a stack

`saddle prs` checks and pushes landed branches, then creates or updates their
pull requests. Three mutually exclusive modes let you inspect or run part of
that operation:

- `saddle prs --dry-run` prints every layer's task, branch, PR base, planned
  head, whether its commits need to be recut, and the configured gate commands.
  It uses local landing state and the cached base snapshot. It does not fetch,
  reconcile state, run hooks or gates, move refs, push, or update PRs. Git may
  write unreachable commit objects to compute the preview.
- `saddle prs --push-only` runs the usual gates and publication holds, then
  pushes the eligible branches. It does not create or edit PRs or comments.
- `saddle prs --gate-only` runs the configured checks against the planned
  stack heads in temporary worktrees. It records gate results without moving
  permanent branches, pushing, or contacting GitHub. A failure still exits
  with an error and its diagnostic output.

Use `--json` with any mode for machine-readable results. A dry run is a
snapshot: fetching a newer base or changing local landing state can change
the next publication's plan.
