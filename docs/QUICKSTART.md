# Quickstart: point Saddle at a repo

This gets you from a fresh clone of Saddle to agents working on an epic in
your own repo. Saddle runs in-process: every `saddle` command opens the repo's
SQLite state in `.saddle/state.db`, and agents live in a hidden tmux session.
There is no daemon to start.

## 1. Install

You need Go, git, [gh](https://cli.github.com), tmux and
[Claude Code](https://claude.com/claude-code) on your PATH.

```sh
git clone https://github.com/brandonapol/saddle && cd saddle
make setup      # dev tools; asks for a Jev key / Claude login only if missing
make install    # builds saddle into $GOBIN; agents run this binary
```

### Or: install through the Claude Code plugin

To orchestrate from your own Claude Code session instead of the TUI, install
the plugin:

```text
/plugin marketplace add brandonapol/saddle
/plugin install saddle@saddle
```

The plugin needs the `saddle` binary on your PATH, at least at the plugin's
version. The supported way to get it is `go install` of that release tag:

```sh
go install github.com/brandonapol/saddle/cmd/saddle@v<plugin version>   # e.g. @v0.1.0
```

and `$(go env GOPATH)/bin` on your PATH. You don't have to remember this: at
session start the plugin checks the binary and, when it is missing or older
than the plugin, prints the exact command above (or, without Go, how to get
Go first). It never installs anything by itself. To install, run the printed
command, ask Claude to run it, or run `saddle plugin install --yes` (or the
plugin's `bin/saddle-check --yes` when saddle is missing), which prints the
command and then runs it. Dev builds (`make install`) are never flagged.

The first time you use `/saddle:orchestrate` or `/saddle:status` in a repo
that never ran `saddle init`, the plugin runs `saddle init` and
`saddle doctor` for you (steps 3 and 4 below) and shows the doctor table.
It goes on only when no check fails; warnings are shown but don't block. Fix
any failures and run the command again. It records this in
`.saddle/plugin-onboarded`, so it happens once per repo, and it leaves repos
that already ran `saddle init` alone. Restart Claude Code once afterwards so
the plugin's MCP tools load for the new repo.

## 2. Stay up to date

```sh
make upgrade    # fast-forwards main from origin and reinstalls
```

It refuses to run unless you are on a clean `main`, so it never discards
local work.

## 3. Initialize your repo

```sh
cd your-repo
saddle init
```

It greets you with a small howdy banner. `--quiet` or a non-terminal stdout
hides it, and `NO_COLOR` prints it without color. It also writes
`.saddle/config.toml`, adds `/.saddle/` to `.git/info/exclude`, detects a
test command (`make check`, `go test ./...`, `npm test`, `cargo test`) and
installs the ref guard hooks, which stop anyone but the merge train from
moving Saddle's branches.

## 4. Run the doctor

```sh
saddle doctor          # a table: ok / warn / FAIL, then a fix for each problem
saddle doctor --json   # the same for scripts
```

It checks the git remote and the repo's default branch (detected, not assumed
to be `main`), gh login and token scopes, repo merge settings (merge commits
off, squash on), branch protection, `test.cmd`, tmux and claude, the ref guard
hooks, that `.saddle/` is ignored, that `state.db` opens and migrates, and how
many stale worktrees and branches `saddle gc` could clean up. It exits
non-zero when any check fails. Warnings alone exit zero. Fix the failures
before your first run.

## 5. Your first epic

```sh
saddle up
```

Then tell the orchestrator in the right-hand chat what to do, e.g.

> here's an epic: #42, plan it and show me before starting

You can also plan from the command line: `saddle plan gh:#42` drafts a plan
under `.saddle/plans/`, and `saddle plan show|edit|approve` reviews it. Once
you approve, the orchestrator starts one agent per task, each in its own
worktree and branch with disjoint path claims. Finished agents call `done`.
The merge train lands their branches one at a time onto `saddle/integration`
(rebase, run `test.cmd`, fast-forward), and `saddle prs` opens stacked PRs
against your default branch.

Quitting the TUI leaves agents running. `saddle up` reconnects and
`saddle down` stops them.

## Holds and auto-merge

**Holds.** Saddle stops work that would go wrong, and always tells you how
to resume it:

- `saddle queue hold <task>` keeps a branch out of the merge train without
  losing its place. `saddle queue release <task>` lets it land.
- A task whose landing failed twice (conflicts, red tests) is escalated to you
  as needs-you instead of being retried.
- When a stack breaks on GitHub, the sentinel flags it. A conflict labels the
  PRs `needs-human`. `saddle sentinel ack` lifts the freeze and
  `saddle unstack <task|pr>` drops a layer.

**Auto-merge** (`saddle automerge on|off|status`, #152) is off by default.
When it is on, Saddle merges the bottom PR of a ready stack with the repo's
allowed method, restacks, and repeats. Ready means required checks passed,
mergeable, not a draft and not labeled `needs-human`. It never merges on red
or pending CI and never bypasses branch protection. It stops and tells the
orchestrator on the first failure. Give your default branch required status
checks (`saddle doctor` warns when it has none) so auto-merge can tell green
from red.
