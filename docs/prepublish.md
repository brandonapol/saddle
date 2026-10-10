# Pre-publish gate worktrees

Checks run against each proposed PR head in a fresh worktree. Configure
`[train] prepublish.setup = "flutter pub get"` (or the repository's dependency
setup command) when checks need dependencies that are not tracked in Git.
Setup runs after checkout, before uncached checks, including after a lockfile
change. It uses the gate's environment and timeout. Changing setup invalidates
cached checks; no setup command preserves the train's existing green cache.

Setup failures hold publication as environment failures. An identical check
failure across every checked stack bottom is also reported as a likely shared
environment problem rather than marking every task red. Commands, worktree
paths, output and held layers appear in `prs` and remain visible in `stack`.
Fix the setup or environment and rerun `prs`; no task repair is required.

Parallel checks register their scratch worktrees one at a time before starting,
so one worker's worktree pruning cannot remove a sibling's new registration.
