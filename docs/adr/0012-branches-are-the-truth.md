# ADR 0012: One task, one branch, one PR: cut from main, fixed in place, merged by a plain queue

- Status: Accepted
- Issue: #930. Supersedes ADR 0003, ADR 0007, ADR 0008 and ADR 0011.
  Amends ADR 0009. Related: #836, #927, fbx67 PR #293.

## Context

Saddle's job is to run agents in parallel and get their work onto main
without anyone doing merge-train toil. On fbx67 on 2026-11-09 it was doing
the opposite:

- Thirteen open PRs, twelve of them in one chain (#272 → … → #293). None can
  merge before the PR under it does. Every PR in the chain shows a red X.
- Three of those PRs (#283, #284, #285) don't contain the PR they're stacked
  on. GitHub shows a stack that the branches don't have.
- PR #293, "Repair t50 Town map doc sync", is one markdown file and one line
  of `drive.ts`. It shows 23 files, +612 lines and 8 commits from other tasks,
  and sits at layer 12. Its original task never had a PR: its branch carried
  commits from unmerged tasks, a rebase conflicted in two files that task
  never touched, and saddle spawned a new task on a new branch, which dropped
  edits to `talk.ts` because `talk.ts` was in a PR four layers down that its
  branch didn't contain.
- The red X on every stacked PR is `saddle/base`, a failing status saddle
  posts on purpose to say "not targeting main". Real CI failures look the
  same.
- In fbx67's log: 546 `stack_warn` ("files outside its claims") events that
  nothing acted on, 316 retargets, 894 `reconcile_shadow` disagreements
  between the two stack engines.

These all come from one choice. Work goes first onto a private local branch,
`saddle/integration`, in the order tasks finish. Saddle then cuts that line
back into per-task commit ranges, *guesses* which PR each range belongs on
(spawn parent, shared issue, a task naming another, "doesn't apply without",
the pre-publish gate), and rebases each guessed branch on its own. The
branches drift from what GitHub shows, and restacks, repairs, freezes,
sentinels, flags and fake statuses exist to patch the drift.

An engineer doing this by hand would branch off main, open a PR against main,
rebase it when main moves, fix its conflicts and red CI on the same branch,
and merge it when it's green. Stacking would be rare and deliberate.

## Decision

1. **A task's branch is cut from base (main) at spawn.** The only exception
   is `after: X` while X is unmerged: then the branch is cut from X's branch.
   Nothing else stacks: not a spawn parent, a shared issue, task text, file
   overlap or a failing gate.
2. **The branch is the change, for life.** One task has one branch and at
   most one PR. A rebase, conflict fix or CI fix moves the branch's head and
   force-pushes the same PR. Saddle never creates a second task, branch or
   PR to carry an existing task's work.
3. **There is no integration branch.** `done` runs the task's gate in its
   worktree, rebases the branch onto its base (main, or its `after`
   branch), pushes it and opens the PR. That is the whole publish step.
   There is no landed list, commit-range slicing, replay, or per-layer
   pre-publish gate. The repo's CI is the gate.
4. **A PR contains only its own task's work.** Before pushing, saddle checks
   that every commit in `base..branch` belongs to the task. If one doesn't,
   publish fails with the commits named. It never ships another task's
   commits.
5. **A plain merge queue merges into main, one PR at a time.** The queue
   takes the oldest PR that targets main, isn't a draft and isn't held by
   the owner. If the PR is behind main, the queue rebases it onto main and
   force-pushes it, then waits for CI. When CI is green and GitHub reports
   it mergeable, the queue merges it with the repo's merge method. A stacked
   PR (rule 1) is never merged while its base isn't main. When its base
   merges, the queue rebases it onto main (`git rebase --onto main
   <old base tip>`) and retargets it to main.
6. **A problem on a PR stays on that PR.** On a rebase conflict or red CI
   the task is reopened on its own branch: its agent is resumed if it's
   alive, otherwise a fresh agent is started *as the same task* in the same
   worktree. After `[merge] fix_attempts` (default 2) the PR gets the
   `needs-human` label and the owner is told once. A problem PR holds only
   itself and the PRs that declared `after` it. It never freezes files,
   blocks publishing, or stops the queue for anyone else. If main itself is
   red, the queue pauses and the owner is told once. No per-PR fixes are
   spawned.
7. **Saddle posts no commit statuses.** A check on a saddle PR is always the
   repo's own CI. Merge order is enforced by the queue (rule 5), not by a
   red check.
8. **One engine.** prgraph (ADR 0009) is the only code that decides
   publishing, rebasing, retargeting and merging. Its edges are only
   `declared` edges from `after`. The `inferred` and `verified` kinds and the
   `parent`, `issue`, `owner_stack`, `hand_retarget` and `gate` sources are
   removed. The legacy layout, restack, landed list, CI-red, repair,
   stuck-stack, flag and batch code is deleted rather than kept behind a
   switch.
9. **Few words.** The vocabulary is: task, branch, PR, `after`, queue,
   merged, conflict, red, `needs-human`. The words *landed*, *layer*,
   *stack* (outside "stacked PR"), *repair*, *repairing*, *superseded*,
   *folded*, *integration branch*, *batch* and *delivery* are retired,
   along with every glossary entry that only exists to tell them apart.

## Alternatives considered

- **Keep the integration branch and fix the layout (ADR 0003 option b, ADR
  0009's shadow cutover).** It has been patched for months (#751, #779, #836,
  #912, #913, #915, #916, #919, #920, #922). The drift comes from cutting a
  linear history back into a graph, so it comes back whatever the patch.
- **Batch mode (ADR 0011).** It merges one long chain at once. Flat PRs
  merge when each is green, so no chain needs to be merged together.
  Removed by the owner's choice.
- **Infer stacks from a spawn parent.** It's a guess, and a wrong guess puts
  unrelated work in one chain. A child that needs its parent's unmerged code
  says `after`.
- **Let a model resolve conflicts on the queue's behalf.** Still rejected
  (ADR 0006). The task's own agent fixes its own branch; the queue's rebase is
  plain git plus the existing deterministic steps (rename map, regenerated
  derived files).

## Consequences

- A PR's diff is its task's work, so it can be reviewed. Most PRs target main
  and merge on their own.
- Two tasks that edit the same lines both open PRs against main. The second
  to merge gets a conflict on its own branch, and its own agent, which knows
  that code, fixes it. That's one more conflict than the serial train
  surfaced at land time, but it lands on the right owner and blocks nobody.
- Workers no longer see siblings' unmerged work. A worker that needs it
  declares `after`. `saddle sync` rebases onto the task's base, not
  integration.
- `saddle land`, `saddle prs`, `restack`, `unstack`, `repair`, `requeue`,
  the `stack` subcommands, batch mode, the `saddle/base` status, the stack
  comment, the stuck-stack and CI-red blocker issues, and the state files
  `stacks.json`, `pr-bases.json`, `published.json`, `ci-red.json`,
  `stack-at-risk.json`, `stuck-stack.json` and `stall-held.json` go away.
  Each removal is its own issue, listed under #930.
- Repos with PRs open under the old layout migrate once with `saddle
  flatten`: each open saddle PR's own commits are replayed onto main on its
  own branch, force-pushed and retargeted to main. A conflict goes through
  rule 6.
- ADR 0009's state machine, Reconcile contract and invariants stay. Its
  edge rules, its shadow rollout and the cutovers #832–#835 are replaced by
  this ADR's issues.
