# What can stop saddle

Saddle is meant to run unattended for hours. This is the catalog of what
stops it, so a run can be checked for them before the owner walks away
instead of finding them one wake-up at a time (#408).

`saddle doctor --predict` runs the cheap probes marked **predict** below and
prints at most 7 ranked risks, each with a fix. `saddle up` runs it in the
background and shows anything Medium or High as one decision card (#404); it
never blocks startup. Risks are weighted by this repo's escalations from the
last 14 days: every escalated blocker (#407) is put in a **class** below, a
class the repo keeps hitting is bumped one level, and escalations that fit no
class are listed as `unknown`. Those are what to add here next.

Code: `internal/doctor/predict.go` (probes, classes, ranking),
`internal/doctor/predict_system.go` (the real machine),
`internal/cli/doctorpredict.go` (the command and the card at `saddle up`).

Each row says how it is handled, linking the code or issue, or says **gap**.

## Third-party checks

| Failure mode | Class | Probe | Handling | Gap |
|---|---|---|---|---|
| A required history-scanning check (GitGuardian, gitleaks, TruffleHog, DCO, commit-message linters) goes red on a commit a later commit fixed | `history_check` | **predict**: required checks from the branch-protection API matched against the known scanners and `[ci] history_checks`; High when `[ci] no_secret_gate` is on | Pre-land secret scan, layer rewrite, one card: `internal/app/historyscan.go`, [docs/history-checks.md](history-checks.md) (#405) | A secret on an already-merged layer needs a human to rewrite history |
| CLA bot, Codecov, Snyk, Vercel/Netlify previews, Sonar or other checks run by an outside service with its own quota or sign-off | `external_check` | **predict**: required checks matched by name | Known flaky checks rerun once: `[ci] flaky` (#234) | **gap**: no quota or sign-off probe; a stuck one shows only as a stuck stack |

## Branch protection

| Failure mode | Class | Probe | Handling | Gap |
|---|---|---|---|---|
| Required reviews with auto-merge on: nothing ever merges | `branch_protection` | **predict**: `gh api repos/:o/:r/branches/<base>/protection` against the auto-merge switch; High | `saddle doctor` reports the approvals count: `internal/doctor/doctor.go` (`protection`) | Auto-merge itself doesn't refuse to start |
| Required signed commits and unsigned agent commits | `branch_protection` | **predict**: `required_signatures` against `git config commit.gpgsign`; High | none | **gap** |
| Merge commits allowed, which breaks stacked PRs (#109) | `branch_protection` | `saddle doctor` (merge settings) | `saddle up` refuses: `app.CheckRepoMergeSettings` in `internal/app` | |
| Linear history, CODEOWNERS | `branch_protection` | none | none | **gap** |

## GitHub

| Failure mode | Class | Probe | Handling | Gap |
|---|---|---|---|---|
| REST rate limit (5000/h) spent by many agents watching PRs | `github_rate_limit` | **predict**: `gh api rate_limit`; Medium under 20% left | The CI watcher backs off while quiet: `internal/app/cired.go` (`[ci] red_max_interval`) | **gap**: no trend, no secondary-limit handling |
| GitHub outage, Actions queue backlog, Actions minutes used up | | none | none | **gap** |

## Credentials

| Failure mode | Class | Probe | Handling | Gap |
|---|---|---|---|---|
| gh logged out, token expired, SSO re-auth | `credentials` | **predict**: `gh auth status`; High | `saddle doctor` and `saddle up` refuse to start: `internal/doctor/doctor.go` (`ghAuth`) | Expiry mid-run is found only when a call fails |
| gh token lacks `workflow`, so pushes touching `.github/workflows` are rejected | `credentials` | **predict**: token scopes against `.github/workflows` files; High when the repo has them | `saddle doctor` warns | |
| ssh-agent or GPG pinentry prompting mid-commit | `credentials` | none | none | **gap**: no dry-run signed commit or agent TTL probe |

## Claude

| Failure mode | Class | Probe | Handling | Gap |
|---|---|---|---|---|
| The 5h window or weekly cap: every agent parks, the orchestrator too | | usage forecast | Usage tracking and limits: `internal/usage`, `internal/app/limits.go`, `internal/forecast` | Not yet a `--predict` line |
| Claude Code auto-updates mid-run and changes stream-json or flags | `claude_cli` | **predict**: `claude --version` against the version the last `saddle up` recorded (`.saddle/claude-version`), Medium on drift; Low while `DISABLE_AUTOUPDATER` is unset | none beyond the warning | **gap**: no version pin |
| Login expiry | | none | none | **gap** |

## Machine

| Failure mode | Class | Probe | Handling | Gap |
|---|---|---|---|---|
| CPU oversubscription (load ~40) times out gates and e2e | `load` | **predict**: load average and concurrency against `nproc`; High when load ≥ CPUs, Medium when concurrency × 2 > CPUs | The heavy-run queue and backpressure: `internal/runq`, [docs/runq.md](runq.md) (#236, #238) | |
| Disk, inode or `/tmp` quota filled by worktrees and builds | `disk` | **predict**: free space and inodes on the worktrees, tmp and the user cache, once per filesystem; High under 5 GB or 100k inodes, Medium under 15 GB | Per-task scratch dirs and environment-failure detection: `internal/scratch`, `internal/app/gateenv.go` (#184, #274); `saddle gc` | |
| OOM killer | `memory` | **predict**: `MemAvailable` per agent, Medium under 1.5 GB | Gate environment failures don't blame the branch: `internal/app/gateenv.go` (#184) | |
| Laptop suspend, lid close, network drop | | none | none | **gap**: no sleep-inhibitor probe |
| tmux server death, reboot | | none | Snapshots of uncommitted work and resume: `internal/app/lifecycle.go`, `saddle resume` (#254) | |

## Repo

| Failure mode | Class | Probe | Handling | Gap |
|---|---|---|---|---|
| Interactive pre-commit hooks | | `saddle doctor` (repo hooks) | `internal/refguard/repohooks.go` | Hooks aren't run non-interactively once to prove it |
| Heavy tests (flutter, e2e) | `load` | **predict** (load) | `internal/runq` (#236) | `make check` isn't timed |
| Tests needing env secrets | | none | none | **gap** |
| Flaky tests driving repair loops | `ci_red`, `repair_of_repair` | history weighting | `[ci] flaky` reruns (#234); a repair of a repair is refused: `internal/app/historyscan.go` (#405); CI repair cap card: `internal/app/cired_repair.go` | No flaky history from the train log |
| Lockfile or generated-file conflicts | `train_escalated` | history weighting | The train returns the branch to its agent, then escalates: `internal/app/train.go` | |

## Saddle

| Failure mode | Class | Probe | Handling | Gap |
|---|---|---|---|---|
| A CI repair goes red itself (repair-of-repair recursion) | `repair_of_repair` | history weighting | Refused and escalated: `internal/app/historyscan.go` (#405) | |
| Stacks stuck with nobody fixing them, restack loops | `stuck_stack` | history weighting | Stuck-stack card: `internal/app/stuckstack.go`; restack journal: `internal/app/restack_journal.go`, [docs/restack-recovery.md](restack-recovery.md) | |
| A branch fails to land too often | `train_escalated` | history weighting | Escalated after `[train] max_attempts`: `internal/app/train.go` | |
| CI still red after the repair attempts | `ci_red` | history weighting | `internal/app/cired_repair.go`, blocker issue (#407) | |
| Respawning killed tasks (#337) | | none | none | **gap** |
| state.db lock or corruption; schema downgrade (#137) | | `saddle doctor` (state.db) | `internal/store/schema.go`, `internal/app/lock.go` | |
| MCP calls hitting the 30-minute timeout | | none | `done` runs its gate in the background and returns a job id: `internal/app/done.go` (#368) | Other long calls |

## Humans

| Failure mode | Class | Probe | Handling | Gap |
|---|---|---|---|---|
| Force-pushes or retargets on saddle's branches | | none | Detected and reconciled: `internal/app/prbase.go` (#359), `internal/sentinel` | |
| Merging by hand out of order, editing a PR saddle owns | | none | Detected by the sentinel: `internal/sentinel` | Documented only |

## Adding a failure mode

1. Add a row here, in its area, with its class.
2. If an escalation already names it, add a cause pattern to `causeClasses`
   in `internal/doctor/predict.go` so history counts it instead of `unknown`,
   and a line to `historyRisks`.
3. If a cheap probe can see it before a run, add one to `Predict` behind
   `PredictEnv`, with a test against the fake in `predict_test.go`.
