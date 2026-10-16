# ADR 0001: The forge layer, one interface over gh and glab

- Status: Proposed
- Issue: #644 (this layer), #647 (the watchers onto it), #645 (GitLab), #646 (provider and CLI detection)

## Context

Saddle talks to its git host by shelling out to `gh` from many packages:
`internal/app` (PR create, retarget, comment, close, issue reads and writes,
merge settings, auth), `internal/automerge`, `internal/ciwatch`,
`internal/sentinel`, `internal/cli` (changelog) and `internal/doctor`. Each
has its own copy of "run gh in dir, wrap stderr into the error". Nothing is
behind an interface, so a repo on GitLab cannot use PRs, CI watching,
auto-merge or stack comments, and adding a provider means touching every
package.

#646 added the `[forge]` config section (`provider = "github" | "gitlab"`,
`cli = "gh" | "glab" | "none"`) and the setup-time detection in
`internal/doctor/forge.go` (`DetectForge`): provider from the origin
remote's host, then the CLI for it, falling back to the other CLI when it is
installed and logged in for that host, else local-only. `saddle init` and
`saddle doctor` report what it picked. Nothing at run time reads `[forge]`
yet.

## Decision

Add `internal/forge`. It owns two things:

1. **The only process start of a forge CLI.** `forge.Output` and
   `forge.CombinedOutput` run `gh` (later `glab`) in a directory.
   Every other package that needs the CLI goes through them, keeping its own
   error wording, so no message changes. A test in `internal/forge` parses
   every non-test Go file in the repo and fails when a package other than
   `internal/forge` passes `"gh"` or `"glab"` to `exec.Command` or
   `exec.CommandContext`. Generic exec helpers that take a program name
   (doctor's `Env.Exec`, the changelog's `changelogExec`) route a forge CLI
   name to `forge.CombinedOutput`/`forge.Output`.

2. **`forge.Forge`, a provider-neutral interface** with `forge.GitHub` as its
   first implementation, built over a `forge.Runner` (the CLI, faked in
   tests). Types carry no GitHub JSON names: `PR{URL, State, Mergeable,
   Base, HeadSHA, Body}`, `Issue{Number, Title, State, Body, URL, Labels,
   Created}`, `Comment{ID, Body}`, `MergeSettings{Repo, URL, Merge, Squash,
   Rebase}`. States keep saddle's existing vocabulary (`OPEN`, `CLOSED`,
   `MERGED`; `MERGEABLE`, `CONFLICTING`, `UNKNOWN`), which a provider maps
   onto. Reads take a field set (`forge.PRFields`, `forge.IssueFields`) so a
   call asks for only what it reads, as it does today.

`internal/app` reaches the forge through `App.Forge()`, which calls
`forge.New(cfg.Forge, root)`.

### Provider selection

Selection reuses #646's `[forge]` section; it adds no new keys.

| `[forge]` | run time (`forge.New`) |
|---|---|
| unset, or `provider = "github"`, `cli = "gh"` | `forge.GitHub` over `gh` (today's behaviour) |
| `provider = "gitlab"` or `cli = "glab"` | not supported until #645: every operation returns `forge.ErrUnsupported` |
| `cli = "none"` | local-only: every operation returns `forge.ErrUnsupported` |

`forge.New` does no detection itself: it runs on hot paths and must not spawn
`gh auth status`. Detection stays a setup-time step (`saddle init`, `saddle
doctor`, `internal/doctor.DetectForge`), whose answer the owner writes into
`[forge]` when the default is wrong. When #645 lands, an unset `[forge]` on a
GitLab remote will select GitLab from the remote's host (the same
`hostProvider` rule doctor uses), which needs no process start.

Callers treat `ErrUnsupported` like any other forge error they already
tolerate (a failed `gh` call): the feature reports "not available" and
saddle carries on. Nothing new fails hard.

### Operations and the capability matrix

Core operations are needed to run stacked PRs at all; a provider must
implement them. Optional ones may return `ErrUnsupported` and saddle
degrades as noted.

| Operation | Interface | Class | GitHub (`gh`) | GitLab (#645, `glab`) | Degrades to |
|---|---|---|---|---|---|
| Create a PR/MR | `CreatePR` | core | `pr create` | `mr create` | — |
| View state, base, head, mergeability, body | `PR` | core | `pr view --json` | `mr view -F json` | — |
| Retarget (change base) | `SetPRBase` | core | `pr edit --base` | `mr update --target-branch` | — |
| Edit body | `SetPRBody` | core | `pr edit --body` | `mr update --description` | — |
| Close | `ClosePR` | core | `pr close` | `mr close` | — |
| List | `ListPRs` | core | `pr list --json` | `mr list -F json` | — |
| Comment, list and edit comments | `AddComment`, `Comments`, `EditComment` | core | `pr comment`, `pr view --json comments`, GraphQL `updateIssueComment` | `mr note`, notes API | stack comments are skipped |
| CI status and logs | `Checks`, `FailedLog`, `PR` with `PRChecks` | core | `pr checks`, `run view --log-failed` | pipelines and jobs API | — |
| Rerun failed CI | `RerunFailed` | optional | `run rerun --failed` | `ci retry` | repair spawns a task instead |
| Merge, auto-merge | `MergePR` | core | `pr merge --squash/--rebase --match-head-commit` | `mr merge --when-pipeline-succeeds` | — |
| Merge settings | `MergeSettings` | optional | `api repos/{owner}/{repo}` | project API (`squash_option`, `merge_method`) | warning, assume squash |
| Branch protection | doctor | optional | `api …/branches/B/protection` | protected branches API | doctor warns |
| Labels | `AddLabel`, `RemoveLabel`, `EnsureLabel` | optional | `pr edit --add-label`/`--remove-label`, `label create --force` | `mr update --label`, labels API | flags shown in saddle only |
| Issues: view, list, create, comment, edit, close, reopen | `Issue`, `ListIssues`, `CreateIssue`, `CommentIssue`, `EditIssueBody`, `CloseIssue`, `ReopenIssue` | optional | `issue …` | `issue …` (`#N` numbering as on GitHub) | autopilot, blocker issues, ticket off |
| Sub-issues | `SubIssues`, `SubIssueNumbers`, `AddSubIssue` | optional | REST `sub_issues` | none (child items are Premium epics) | ticket shows no sub-issues |
| Native stacks | `ghstack` runner | optional | `gh stack` extension | none | saddle chains PR bases itself |
| Auth status | `AuthStatus` | core | `auth status` | `auth status` | doctor reports the fix |
| Repo identity | `MergeSettings.Repo/URL` | core | `api repos/{owner}/{repo}` | project API | — |

GitLab differences #645 must handle behind this interface: MRs are numbered
per project (`!12`) and saddle stores the MR URL as it stores the PR URL
today; draft MRs; squash and fast-forward are project settings; pipelines
rather than check runs; protected-branch rules; self-hosted hosts
(`GITLAB_HOST`).

### Migration

This was done in two refactor steps, with no behaviour change for GitHub
users:

- #644 moved every process start of `gh` into `internal/forge`
  (`forge.Output`, `forge.CombinedOutput`), each caller keeping its exact
  arguments and error wording, and moved `internal/app`'s direct `gh` calls
  behind `forge.Forge`. Test seams in `app` (`prHead`, `prView`, `issueText`,
  `ghStackRun`) stay.
- #647 moved the packages that took an injected gh runner and parsed GitHub
  JSON themselves (`ciwatch`, `sweeper`, `automerge.GH`, `sentinel.GH` and
  `CIRed`, `planner.Push`, `App.GH`) onto typed operations: `Checks`,
  `FailedLog`, `MergePR`, `RemoveLabel`, `SubIssueNumbers`, `AddSubIssue`,
  PR reads with labels, files, merge state and the check rollup
  (`forge.CheckRun`), merged-PR lookup by head branch (`PRQuery.Head`) and
  issue creation in another repo (`NewIssue.Repo`). Each package declares the
  slice of `forge.Forge` it uses and is built from `App.Forge()`, so `[forge]`
  applies everywhere; on `ErrUnsupported` each degrades quietly (no CI
  watching, no labels or stack comments). `forge.GitHub` issues the same gh
  subcommands, flags and values those packages did; the only difference is
  that `--json` field lists come in forge's fixed order, which gh ignores.
  `automerge.GH` keeps a `Run` field (a gh runner) as a compatibility seam for
  callers that have no forge; it runs through `forge.GitHub`.
- `TestNoGHArgsOutsideForge` parses the repo and fails when a package outside
  `internal/forge` builds gh arguments. It allows only GitHub-only code with
  no provider-neutral meaning: `internal/ghstack` (the gh-stack extension,
  which stays optional and GitHub-only), `internal/doctor` (setup-time
  diagnostics of the gh install, branch protection and rate limits) and
  `internal/cli/changelog.go` (release notes for saddle's own GitHub
  releases), plus the e2e fake gh and its harness.
- `internal/e2e/fakegh` stays as is: it is a fake `gh` binary on `PATH`, so it
  fakes the GitHub implementation end to end. #645 adds a fake `glab` the
  same way.

## Consequences

- One place knows how to start the forge CLI, and an enforced test keeps it
  that way.
- A provider implements `forge.Forge`; the matrix above is what GitLab must
  meet. Optional operations returning `ErrUnsupported` degrade features
  rather than failing saddle.
- `[forge] cli = "none"` or `provider = "gitlab"` (until #645) turns off every
  forge call, the CI watcher, sweeper, sentinel, ci-red watcher, auto-merge
  and plan push included. Doctor's diagnostics and gh-stack still assume
  GitHub; they are the allowlisted exceptions above.
