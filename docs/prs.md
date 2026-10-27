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

## When a layer fails the pre-publish gate

A layer is gated on its own commits plus the layers below it. If it fails for
want of something a sibling task adds (a missing symbol, a file), `saddle prs`
names the probable edge and prints the command, for example
`saddle after <task> <dep>`. It never records the edge itself. Run the command
(MCP tool `after`), then run `saddle prs` again: the task now stacks on the
dep even when their files don't overlap. `saddle after` writes
`.saddle/deps/<task>`, the file `spawn --after` writes, and works on a task
that is already spawned (#675).

A dry run is a snapshot: fetching a newer base or changing local landing state
can change the next publication's plan.
