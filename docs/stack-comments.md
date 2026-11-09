# PR descriptions

`prs` creates a PR with the task's summary and leaves its description alone on
later runs, including descriptions edited by a reviewer. Put any intended
closing-issue directive in the task summary itself.

Saddle posts no stack comment and no commit status on its PRs (ADR 0012,
#931). A check on a saddle PR is always the repo's own CI. Comments and
`saddle/…` statuses that older versions posted stay on GitHub untouched.

When gc deletes a merged task's branch, it first moves every open PR still
based on that branch onto base. GitHub closes a PR whose base branch is
deleted by a push, rather than retargeting it.
