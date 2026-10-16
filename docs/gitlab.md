# GitLab

Saddle runs on GitLab (gitlab.com and self-hosted) through the
[GitLab CLI](https://gitlab.com/gitlab-org/cli), `glab` (#645). It opens
stacked merge requests, retargets them as the stack moves, watches their
pipelines, repairs red CI, auto-merges, and keeps its stack notes on them,
the same as it does with pull requests on GitHub. The design is in
[ADR 0001](adr/0001-forge-layer.md).

## Setup

1. Install `glab` and log in for the host your remote is on:

   ```sh
   glab auth login --hostname gitlab.com      # or your own host
   ```

2. Make sure saddle knows the repo is on GitLab. A remote on gitlab.com, or
   on any host with "gitlab" in its name, is detected. For another
   self-hosted host, say so in `.saddle/config.toml`:

   ```toml
   [forge]
   provider = "gitlab"
   host = "git.example.com"   # only when it differs from the remote's host
   ```

   `host` is passed to every `glab` call as `--hostname`; without it, glab
   takes the host from the remote, or from `GITLAB_HOST`.

3. Set the project's merge settings (Settings -> Merge requests). Saddle
   stacks merge requests and needs linear history:

   - **Merge method: Fast-forward merge.** "Merge commit" and "Merge commit
     with semi-linear history" both make merge commits; `saddle up`, `land`
     and `prs` refuse to run on them, as they do on a GitHub repo that
     allows merge commits.
   - **Squash commits when merging: Allow, Encourage or Require.** Saddle
     squashes when the project allows it. With "Do not allow" it merges
     fast-forward, keeping each commit.
   - **Pipelines must succeed** is recommended. Saddle's auto-merge waits
     for green pipelines itself, but the setting also stops a person from
     merging red work.

4. Protect the default branch (Settings -> Repository -> Protected
   branches) so only merge requests change it.

5. Run `saddle doctor`. The git provider row should read
   `GitLab (gitlab.com) via glab …, logged in to gitlab.com`. Its merge
   settings row checks step 3 (fast-forward merge, squash allowed) and its
   branch protection row checks step 4 and "Pipelines must succeed",
   through `glab api`. Reading them needs Maintainer access; without it
   the rows warn and you check by hand.

## How saddle maps GitLab

| Saddle | GitHub | GitLab |
|---|---|---|
| PR | pull request `#12` | merge request `!12` (numbered per project) |
| PR link stored on a task | `…/pull/12` | `…/-/merge_requests/12` |
| draft | draft PR | title starting `Draft:` |
| retarget a stacked PR | `gh pr edit --base` | the MR's target branch |
| stack comment ([stack comments](stack-comments.md)) | PR comment | MR note; GitLab's system notes are ignored |
| `Closes #N` in the description | closes the issue on merge | the same, as GitLab reads it |
| CI checks | check runs and commit statuses | the jobs of the MR's head pipeline; a job's stage reads as its workflow |
| failed log | `gh run view --log-failed` | the job's trace, without colors and section markers |
| rerun failed CI | `gh run rerun --failed` | retry the pipeline |
| merge state | `mergeStateStatus` | `detailed_merge_status` (below) |
| merge | `gh pr merge --squash --match-head-commit` | merge API, `squash`, pinned to the head with `sha` |
| sub-issues of an epic | sub-issues | linked issues ("relates to"); child items are a paid feature |
| labels (`ci-red`, `saddle:needs-human`) | labels | labels, created on first use |

Saddle's auto-merge merges a merge request only when GitLab says it is
`mergeable` and its head pipeline passed. The other states hold it with a
reason in `saddle automerge` status:

| `detailed_merge_status` | reads as |
|---|---|
| `mergeable`, pipeline passed or none | CLEAN: merges |
| `mergeable`, pipeline failed or still running | UNSTABLE or BLOCKED: waits |
| `conflict` (or `has_conflicts`) | CONFLICTING: "it conflicts with main" |
| `need_rebase` | BEHIND: saddle's restack rebuilds the branch |
| `draft_status` | a draft |
| `ci_must_pass`, `ci_still_running`, `not_approved`, `discussions_not_resolved`, `blocked_status` and the rest | BLOCKED: waits for the pipeline, approvals or threads |
| `checking`, `unchecked`, `preparing` | GitLab still computing: waits |

Required approvals are never bypassed: an unapproved merge request stays
BLOCKED until someone approves it. When a merge call finds the pipeline
still running, saddle asks GitLab to merge when it succeeds, and GitLab
merges it on its own.

Jobs marked `allow_failure` don't turn CI red. Manual jobs count as
skipped, since they run only when someone starts them.

## What isn't there yet

- **gh-stack** (`stack_backend = "gh-stack"`) is a GitHub extension.
  On GitLab saddle chains MR targets itself, the default backend.
- **Automatic reruns of flaky CI.** The ci-red watcher reruns a failure
  it thinks is infra or flaky by its GitHub Actions run id; on GitLab it
  spawns a repair task instead.
- **PR numbers.** Commands that take a task, PR URL or PR number
  (`saddle unstack`, `automerge hold`/`release`, `collapse`, `repair`) find
  a PR by number through GitHub's `/pull/12`; on GitLab give them the task
  id or the MR's full URL. The TUI's auto-merge view and the notice digest
  show the full MR URL where GitHub shows `#12`.

## Real-run checklist (for the owner)

The fake `glab` in the e2e journeys (`internal/e2e/fakeglab`,
`internal/e2e/journeys_gitlab_test.go`) speaks the REST API as documented;
it has not yet been checked against gitlab.com. On a throwaway gitlab.com
project with a one-job `.gitlab-ci.yml` (say `test: script: [test ! -e FAIL]`):

1. Set the project to Fast-forward merge, squash Encourage, Pipelines must
   succeed; protect `main`. `glab auth login`. Clone it, `saddle init`;
   `saddle doctor` shows `GitLab (gitlab.com) via glab`.
2. Spawn two small tasks on disjoint files, `saddle land`, `saddle prs`:
   two MRs, the second targeting the first's branch, with a saddle stack
   note on it. Check the stored PR links (`saddle status`).
3. `saddle publish <task>` on a third task: one MR against `main`;
   publishing again reports the existing one.
4. Make the bottom MR's pipeline fail (commit `FAIL`): the ci-red watcher
   labels both MRs `ci-red`, spawns a repair with the job's trace in its
   prompt; once it lands and the new pipeline passes, the labels go.
5. `saddle automerge on`: the bottom MR is squash-merged, the top one is
   retargeted to `main`, rebuilt, and merged after its pipeline.
6. Push a conflicting change to `main` by hand: auto-merge holds the MR
   with "it conflicts with main".
7. Turn on required approvals: auto-merge holds an unapproved MR as
   BLOCKED and merges it once approved.
8. `saddle plan push` an approved plan: an epic issue with its tasks as
   linked issues; pushing again links nothing twice.
9. A self-hosted instance if one is handy: `[forge] provider = "gitlab"`,
   `host = …`.

Note any `glab api` error text that differs from the fake's (409 "already
exists" on a second MR from one branch, 406 on a conflict, 405 on a
blocked merge) in #645.
