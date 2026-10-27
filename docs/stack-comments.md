# Stack notes and author descriptions

`prs` creates a PR with the task's summary and leaves its description alone on
later runs, including descriptions edited by a reviewer. Stack order, merge
instructions and the actual base branch go in one comment marked
`<!-- saddle:stack -->`, updated in place when the layout changes. Put any
intended closing-issue directive in the task summary itself.

The comment also says why the PR is stacked, which PR to merge first, and the
PR's size against base: commits and files, and how many of those commits
belong to the layers below it (#751).

## The saddle/base status

Every PR saddle publishes gets a `saddle/base` commit status at its head. It
is green when the PR targets base and red while it targets another task's
branch, because merging it then puts its work on that branch, not on base.
Saddle's own merges (automerge, the sweeper) only merge PRs on base. To stop
anyone else merging into a task branch, add a ruleset for `saddle/**` that
requires the `saddle/base` check. Saddle itself only pushes to those
branches, so the ruleset must allow saddle's pushes or bypass them.

When gc deletes a merged task's branch, it first moves every open PR still
based on that branch onto base. GitHub closes a PR whose base branch is
deleted by a push, rather than retargeting it.
