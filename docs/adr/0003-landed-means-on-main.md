# ADR 0003: Landing opens the PR; only merged into base is done

- Status: Superseded by [ADR 0012](0012-branches-are-the-truth.md)
- Issue: #819. Related: #805 (glossary and ADRs), #779 (prgraph engine)

## Context

On 2026-11-05 seven tasks (#763, #780, #787, #764, #733/#767, #788/#789,
#807) were marked `landed` and the orchestrator reported "wave one is done".
None had a PR. `saddle land` fast-forwarded the local `saddle/integration`
branch and, by design, "never pushed and never touched GitHub": publishing
was a separate `saddle prs` step that someone had to remember. Nobody ran
it, so auto-merge (which only merges PRs) had nothing to merge, nothing
reached `main`, and the installed binary stayed stale. PRs #810-#816 were
opened by hand to recover.

The owner's rule: **main is the only thing that matters.** No work is done
until it is on main, and saddle must not depend on a human or the
orchestrator remembering a publish step.

## Decision

1. **Landing ends with an open PR.** `LandRecorded` (behind `saddle land`,
   the MCP `land` tool and autopilot) runs the train and then opens the PR
   of every landed task that has none, against base, through the same code
   as `prs`. A PR stacks on another task's PR only when the task depends on
   it (declared with `after`, named in its prompt, or needed by the
   pre-publish gate); otherwise it targets base directly. With auto-merge on,
   that PR is carried to base without any further step. A PR that can't
   open yet (red pre-publish gate, red CI below it, a flagged stack, a
   dependency waiting to merge, GitHub down) does not undo the landing: the
   land result says why, and the alarm below fires.
2. **"Merged" is the only finish line.** Every landed task carries a
   *delivery*: `landed locally, no PR yet`, `landed locally, PR open`, or
   `merged` once its PR merged into base (`app.Delivery`). `saddle status`,
   the status tool (`delivery`), the TUI header, the landed notice, the
   digest and the orchestrator brief use those words. The brief tells the
   orchestrator to count work done only when delivery says merged.
3. **A landed task with no PR is an alarm that heals itself.** The sentinel
   calls `PublishLanded` every cycle; each task still without a PR afterwards
   gets one `landed_no_pr` event and an action notice to the orchestrator
   with the cause. `saddle doctor` warns about them ("landed without a PR").
   Because the sentinel and every land run it, the first run of the new
   saddle opens the PRs of tasks an older saddle landed without one: that is
   the migration, and it needs no flag or manual step.
4. **`saddle/integration` stays, as an internal detail of the train**
   (option b of #819), not removed (option a).

## Why keep the integration branch (b) rather than remove it (a)

The issue preferred removing it if stacking and drift allowed. They don't,
without a rewrite that would put the in-flight stacks at risk:

- The train tests each queued branch rebased on top of everything landed
  before it but not yet merged. That base has to be a ref; without one the
  train would test against `main` and land work that only passes alone.
- Every landed task's range (`From..To` in its train note) is recorded on
  that branch, and `restack`, `drift`, the sentinel's layer checks,
  `requeue`, `repair` and the PR layout (`prs --dry-run`) all read those
  ranges. Re-keying them onto per-PR branches is the prgraph work in #779;
  doing it here would duplicate and collide with it.
- Agents' `saddle sync` rebases onto it so a worker picks up landed
  siblings before they merge.

So the branch stays, but it is no longer a destination: it is never shown
as where work goes (the TUI header shows `→ main`, notices say "landed
locally"), the docs describe it as the train's scratch base, and nothing
calls work on it done. #779 can remove it once the stack is keyed on PR
branches.

## Consequences

- `saddle land` now pushes and opens PRs whenever the repo has a remote.
  The glossary's "land never pushes" promise is gone. A repo with no remote
  lands locally only, and its land results say so.
- `saddle prs` remains, for refreshing the stack, `--dry-run`,
  `--push-only` and `--gate-only`, and to see why a PR didn't open; nobody
  needs it to get work to base.
- Autopilot no longer runs its own `prs` after landing; land does it.
- An unreachable forge leaves tasks at `landed locally, no PR yet` and
  raises one alarm per task; the sentinel retries every cycle.
