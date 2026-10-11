# History-scanning CI checks

Some required checks read every commit in a pull request, not the tree at
its head: GitGuardian, gitleaks-action, TruffleHog, DCO/sign-off bots and
commit-message linters. When one of them fails, a commit that fixes the tree
doesn't help, because the bad commit is still in the PR's history. A "Fix
CI" task that adds commits can't turn the check green. In #405 that mistake
stalled a stack for about five hours: a fix, then a fix for the fix, then
scanner ignore files nobody could prove were read.

Saddle handles these checks in three places.

## The land gate scans each commit

Before a branch lands, the train scans the lines each of its commits adds
(not only `HEAD`) for secret-shaped literals. It uses the patterns in
`internal/secretscan`, plus `gitleaks` when it's on `PATH`. A hit sends the
branch back to its agent:

> commit abc123 adds a secret-shaped literal at internal/remote/scrub_test.go:42; build it at runtime and squash.

Build the value at runtime (`"gh" + "p_…"`), then squash the branch so no
commit holds the whole literal. To mark a false positive, put a
`gitleaks:allow` comment on its line. `[ci] no_secret_gate = true` turns the
gate off.

## A red history check rewrites the layer

The ci-red watcher sorts each failing check by its `workflow / job` label.
The known history scanners are always on the list. To add your own, give
substrings in `.saddle/config.toml`:

```toml
[ci]
history_checks = ["secret-sweep", "signed-commits"]
```

When a history check is red on a stacked layer, saddle doesn't spawn a fix
task:

- **Unmerged saddle layer.** Saddle owns the branch, so it rewrites the
  layer: the layer's commits are squashed into one with the same tree, the
  layers above are replayed onto it, and the branches are force-pushed with
  lease. The rewrite is logged as a `history_rewrite` event and an info
  notice.
- **The tree still holds the literal.** Commits have to fix the tree first,
  so the ordinary CI repair runs. Its commits are folded into the layer, and
  the next red head gets squashed.
- **The finding can't be rewritten away.** This covers a layer saddle can't
  tell apart, a single clean commit (the finding is in a merged commit or in
  someone else's), and a layer that's still red on the head saddle already
  rewrote it to. Saddle raises one decision card,
  `dismiss in dashboard: <link>`, and stops retrying that layer.

## No repair of a repair

A CI repair whose own PR goes red never gets a repair of its own. The block
works two ways:

- **By title.** Saddle refuses to spawn any task titled
  `Fix CI for <id> Fix CI for …` and gives the reason.
- **By lineage.** If a red layer or a red PR belongs to a task that was
  spawned as a CI repair, saddle escalates it to the orchestrator and
  doesn't spawn anything.

## Ignore files need proof

A scanner ignore file (`.gitguardian.yaml`, `.gitleaksignore`,
`.gitleaks.toml`, …) only counts as a fix once the check passes after the
push. If a layer adds one and the history check is still red, saddle says
that the ignore file isn't a fix, and the rewrite or card above goes ahead.
