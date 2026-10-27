# ADR 0001: The forge layer, one interface over gh and glab

- Status: Accepted
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
`saddle doctor` report what it picked. Before this layer nothing at run
time read `[forge]`.

## Decision

Add `internal/forge`. It owns two things:

1. **The only process start of a forge CLI.** `forge.Output` and
   `forge.CombinedOutput` run `gh` or `glab` in a directory.
   Every other package that needs the CLI goes through them, keeping its own
   error wording, so no message changes. A test in `internal/forge` parses
   every non-test Go file in the repo and fails when a package other than
   `internal/forge` passes `"gh"` or `"glab"` to `exec.Command` or
   `exec.CommandContext`. Generic exec helpers that take a program name
   (doctor's `Env.Exec`) route a forge CLI
   name to `forge.CombinedOutput`/`forge.Output`.

2. **`forge.Forge`, a provider-neutral interface** with `forge.GitHub` and
   `forge.GitLab` (#645) as its implementations, each built over a
   `forge.Runner` (the CLI, faked in tests). Types carry no GitHub JSON names: `PR{URL, State, Mergeable,
   Base, HeadSHA, Body}`, `Issue{Number, Title, State, Body, URL, Labels,
   Created}`, `Comment{ID, Body}`, `MergeSettings{Repo, URL, Merge, Squash,
   Rebase}`. States keep saddle's existing vocabulary (`OPEN`, `CLOSED`,
   `MERGED`; `MERGEABLE`, `CONFLICTING`, `UNKNOWN`), which a provider maps
   onto. Reads take a field set (`forge.PRFields`, `forge.IssueFields`) so a
   call asks for only what it reads, as it does today.

`internal/app` reaches the forge through `App.Forge()`, which calls
`forge.New(cfg.Forge, root)`.

### Provider selection

Selection reuses #646's `[forge]` section; #645 added one key, `host`.

| `[forge]` | run time (`forge.New`) |
|---|---|
| `cli = "none"` | local-only: every operation returns `forge.ErrUnsupported` |
| `provider = "gitlab"` or `cli = "glab"` | `forge.GitLab` over `glab`, with `--hostname host` when `host` is set |
| `provider = "github"` or `cli = "gh"` | `forge.GitHub` over `gh` |
| unset, origin's host names GitLab | `forge.GitLab` over `glab` |
| unset, any other origin | `forge.GitHub` over `gh` (the behaviour before this layer) |

A provider with the other provider's CLI (`provider = "github"`, `cli =
"glab"`) is a config error: neither CLI drives the other host.

`forge.New` does no detection that starts a process: it runs on hot paths
and must not spawn `gh auth status`. It reads the origin's URL from git's
config file (through a worktree's `.git` file and `commondir`) and applies
the same host rule doctor uses (`forge.HostProvider`). Login and CLI checks
stay a setup-time step (`saddle init`, `saddle doctor`,
`internal/doctor.DetectForge`), whose answer the owner writes into
`[forge]` when the default is wrong, for example a self-hosted GitLab whose
host name doesn't say "gitlab".

Callers treat `ErrUnsupported` like any other forge error they already
tolerate (a failed `gh` call): the feature reports "not available" and
saddle carries on. Nothing new fails hard.

### Operations and the capability matrix

Core operations are needed to run stacked PRs at all; a provider must
implement them. Optional ones may return `ErrUnsupported` and saddle
degrades as noted.

| Operation | Interface | Class | GitHub (`gh`) | GitLab (`glab api`, REST v4) | Degrades to |
|---|---|---|---|---|---|
| Create a PR/MR | `CreatePR` | core | `pr create` | `POST merge_requests`; a draft is a `Draft:` title; a 409 "already exists" returns the open MR | — |
| View state, base, head, mergeability, body | `PR` | core | `pr view --json` | `GET merge_requests/:iid` (+ `/diffs` for files) | — |
| Retarget (change base) | `SetPRBase` | core | `pr edit --base` | `PUT merge_requests/:iid target_branch` | — |
| Edit body | `SetPRBody` | core | `pr edit --body` | `PUT … description` | — |
| Close | `ClosePR` | core | `pr close` | `PUT … state_event=close` | — |
| List | `ListPRs` | core | `pr list --json` | `GET merge_requests?state=&source_branch=` | — |
| Comment, list and edit comments | `AddComment`, `Comments`, `EditComment` | core | `pr comment`, `pr view --json comments`, GraphQL `updateIssueComment` | notes API; system notes left out; a comment id is `iid:note` | stack comments are skipped |
| CI status and logs | `Checks`, `FailedLog`, `PR` with `PRChecks` | core | `pr checks`, `run view --log-failed` | head pipeline's `pipelines/:id/jobs`, `jobs/:id/trace` | — |
| Rerun failed CI | `RerunFailed` | optional | `run rerun --failed` | `POST pipelines/:id/retry`, or `POST jobs/:id/retry` for a job link whose pipeline isn't known | repair spawns a task instead |
| Merge, auto-merge | `MergePR` | core | `pr merge --squash/--rebase --match-head-commit` | `PUT merge_requests/:iid/merge squash= sha=`; `merge_when_pipeline_succeeds` while the pipeline runs | — |
| Merge settings | `MergeSettings` | optional | `api repos/{owner}/{repo}` | `GET projects/:id` (`merge_method`, `squash_option`) | warning, assume squash |
| Branch protection | `ReadBranchProtection` | optional | `api …/branches/B/protection` | `GET protected_branches/:name`, then `projects/:id` (`only_allow_merge_if_pipeline_succeeds`, `approvals_before_merge`) | doctor warns "skipped" |
| Default branch | `ReadDefaultBranch` | optional | `repo view --json defaultBranchRef` | `GET projects/:id` (`default_branch`) | doctor uses the remote's HEAD only |
| Merged PRs by base | `ReadMergedPRs` | optional | `pr list --state merged --base` | none (`saddle changelog` refuses) | release notes need GitHub |
| API rate limit | `ReadRateLimit` | optional | `api rate_limit` | none (no quota saddle spends) | no rate-limit risk |
| Labels | `AddLabel`, `RemoveLabel`, `EnsureLabel` | optional | `pr edit --add-label`/`--remove-label`, `label create --force` | `PUT … add_labels/remove_labels`, labels API (create, else update) | flags shown in saddle only |
| Issues: view, list, create, comment, edit, close, reopen | `Issue`, `ListIssues`, `CreateIssue`, `CommentIssue`, `EditIssueBody`, `CloseIssue`, `ReopenIssue` | optional | `issue …` | issues and issue notes API (`#N` numbering as on GitHub) | autopilot, blocker issues, ticket off |
| Sub-issues | `SubIssues`, `SubIssueNumbers`, `AddSubIssue` | optional | REST `sub_issues` | linked issues (`issues/:iid/links`), since child items are Premium | ticket shows no sub-issues |
| Native stacks | `ghstack` runner | optional | `gh stack` extension | none | saddle chains PR bases itself |
| Auth status | `AuthStatus` | core | `auth status` | `auth status [--hostname]` | doctor reports the fix |
| Repo identity | `MergeSettings.Repo/URL` | core | `api repos/{owner}/{repo}` | `GET projects/:id` | — |

### GitLab differences (#645)

`forge.GitLab` makes every call through `glab api` rather than `glab mr`
subcommands: the REST API's JSON and parameters are stable across glab
releases, while `glab mr` flags are not (`--when-pipeline-succeeds` became
`--auto-merge`) and its output is meant for people. Projects are addressed
with glab's `:id` placeholder, which glab fills in from the checkout's
remote; another project is its URL-encoded path. The differences it
handles behind the interface:

- **MR numbers.** MRs are numbered per project and written `!12`. Saddle
  stores the MR's URL (`…/-/merge_requests/12`) where it stores a PR URL;
  a ref may be the URL, `!12`, `12` or a source branch. A few app-level
  helpers still find a PR by number through `/pull/N` (`taskByRef`, the
  TUI, the digest), so on GitLab those take the task id or full URL.
- **Draft MRs** are a `Draft:` title prefix.
- **Merge method and squash are project settings**, not per merge. Saddle
  needs linear history, which is the "Fast-forward merge" method, so
  `MergeSettings` reads `merge_method` "merge" and "rebase_merge" as merge
  commits allowed (saddle refuses to run, as on GitHub) and `squash_option`
  "never" as squash off. `MergePR` sends `squash` for "squash" and lets
  the project's method apply otherwise.
- **Pipelines, not check runs.** A PR's checks are the jobs of the MR's
  head pipeline (the jobs API leaves retried jobs out), with the job's
  stage as its workflow. An `allow_failure` job that failed reads as
  NEUTRAL; a manual job reads as SKIPPED so it never holds CI.
- **Merge state.** `detailed_merge_status` maps onto GitHub's
  `mergeStateStatus` words auto-merge reads: `mergeable` is CLEAN unless
  the head pipeline failed (UNSTABLE) or still runs (BLOCKED), `conflict`
  is DIRTY with mergeable CONFLICTING, `need_rebase` BEHIND, `draft_status`
  DRAFT, `checking`/`unchecked`/`preparing` UNKNOWN, and every other block
  (`ci_must_pass`, `not_approved`, `discussions_not_resolved`,
  `blocked_status`…) BLOCKED. Required approvals are never bypassed.
- **Auto-merge.** Saddle's watcher merges only CLEAN MRs, so it never
  relies on GitLab's auto-merge; when a merge call finds the pipeline still
  running it sets `merge_when_pipeline_succeeds` and returns an error
  saying the MR is scheduled, not merged, and GitLab merges it once the
  pipeline passes.
- **Protected branches** apply to every merge; saddle has no bypass on
  either provider. Doctor reads them through `ReadBranchProtection` (#648).
- **Self-hosted hosts.** An origin whose host names GitLab is detected;
  otherwise `[forge] provider = "gitlab"`, and `host` passes `--hostname`
  to glab. glab's own `GITLAB_HOST` works too.
- **Stack comments** are MR notes; GitLab's system notes ("changed target
  branch…") are left out of `Comments`. "Closes #N" in a description
  closes the issue on merge, as on GitHub.

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
  `automerge.GH` is built from `App.Forge()` everywhere (its old `Run` gh-runner
  seam is gone, #649).
- `TestNoGHArgsOutsideForge` parses the repo and fails when a package outside
  `internal/forge` builds gh arguments. Its allowlist is only
  `internal/forge`, the layer that builds them. #649 moved automerge and the
  e2e queue journey onto the forge; `internal/ghstack`, `internal/e2e` and
  `internal/bench` are no longer exempt, so a gh argument list there fails
  the test. `internal/ghstack` stays optional and GitHub-only (`ghstack.For`
  gates it on the forge's provider). `saddle changelog` lists merged PRs
  through the optional `ReadMergedPRs` and refuses clearly on a forge
  without it (#648).
- `internal/e2e/fakegh` stays as is: it is a fake `gh` binary on `PATH`, so it
  fakes the GitHub implementation end to end. #645 added
  `internal/e2e/fakeglab`, a fake `glab` the same way: a stateful GitLab
  REST API over the World's bare origin. The GitLab journeys
  (`journeys_gitlab_test.go`) run with `[forge] provider = "gitlab"` and
  fail if saddle calls the fake gh at all.

## Consequences

- One place knows how to start the forge CLI, and an enforced test keeps it
  that way.
- A provider implements `forge.Forge`; the matrix above is what GitLab must
  meet. Optional operations returning `ErrUnsupported` degrade features
  rather than failing saddle.
- `[forge] cli = "none"` turns off every forge call, the CI watcher,
  sweeper, sentinel, ci-red watcher, auto-merge and plan push included.
  On GitLab they all run through glab, doctor included (#648). gh-stack
  stays GitHub-only by design: `ghstack.For` gates it on the forge's
  provider, and doctor doesn't probe it elsewhere.
