# Performance

## `saddle up` startup (#635)

`saddle up` used to take about ten seconds before the TUI was usable. Now it
draws its first frame in well under half a second. Nothing it checked was
dropped: the slow checks run after the first frame, and anything that used
to stop `up` now opens a decision card.

### Tracing it

```sh
saddle up --trace-startup                       # print the trace when up exits
SADDLE_TRACE_STARTUP=1 saddle up                # the same
SADDLE_TRACE_STARTUP=/tmp/up-trace saddle up    # also append each stage to the file as it happens
```

The trace lists each stage with when it ended (ms since the process
started) and how long it took. Stages tagged `[after first frame]` run in
the background, beside the TUI. They show up only if they finish before
`up` exits; the full doctor takes about 30s under load. Use the file form to
watch the trace while the TUI is on screen.

```
saddle up startup trace (ms since process start):
       at      took  stage
      0ms       0ms  args, trust
      6ms       6ms  config, state.db open
     21ms      14ms  saddle init
     62ms      40ms  doctor (local checks)
     62ms       0ms  engine lock
     68ms       5ms  stale leases
     68ms       0ms  before TUI
    281ms     213ms  first frame
    282ms       0ms  watchers start [after first frame]
   1789ms    1500ms  base fetch [after first frame]
```

`before TUI` to `first frame` covers starting the orchestrator process,
loading the last 300 chat lines and Bubble Tea's first render.

### Before and after

These are times to the first usable screen, from exec until the TUI
showed, polled from a tmux pane. The machine had 8 CPUs at load 11 to 16,
which is normal for it. The orchestrator command was stubbed, so no real
agent started.

| Repo | Before | After |
|---|---|---|
| This repo: 328 tasks in state.db, 70 branches, real GitHub remote | 12.6s, 13.4s | 0.35 to 0.38s (4 runs) |
| Fresh one-commit repo, same remote, first run and later runs | 5.6s | 0.16 to 0.17s |

Where the old ten seconds went, measured stage by stage on this repo under
load:

| Stage (before) | Cost | Now |
|---|---|---|
| Doctor, run in full before the TUI | 32 to 36s, cut off at the 10s timeout | Local checks only, 40ms. The full doctor runs after the first frame. |
| ↳ leftovers count (`GCCounts`, walks every branch and worktree) | 23 to 25s | after first frame |
| ↳ push dry-run (`saddle can push`) | 5.7s | after first frame |
| ↳ `gh api repos/{owner}/{repo}`, protection, auth, gh stack | 1.5 to 4s together | after first frame |
| ↳ scratch dir walk | 1.0s | after first frame |
| Merge-settings check (`gh api`) | 0.45 to 2.6s | after first frame (with `--skip-doctor`; otherwise the doctor covers it) |
| Base fetch for the "local base is behind" warning | 0.65s | after first frame, as a notice to the orchestrator |
| Base fetch in `Orchestrator()` for the integration-behind notice | about 1s | after first frame (`CheckIntegrationBehind`) |
| Watchers: sentinel, CI, auto-merge, resume (#254) and the rest | started before the TUI, competing for CPU | started on the first frame |

### What runs where

Before the first frame, `up` does only what drawing the TUI or refusing to
start needs:

- trust gate and args
- open the config and state.db
- `saddle init`'s local setup
- the doctor's local checks (`doctor.RunLocal`). These still stop `up` on a
  failure.
- the engine lock
- reaping stale heavy-run leases
- starting the orchestrator, which fetches base only when the integration
  branch doesn't exist yet

After the first frame (`internal/cli/up_deferred.go`):

- **The full doctor** (`doctor.Run`). A failing check opens one
  `doctor-failed` card naming the checks and their fixes, and warnings are
  recorded as a `doctor_warnings` event. Before #635 a failure stopped `up`.
  Now `up` starts, and each command that needs the failing check refuses on
  its own: `land` and `prs` refuse merge commits, a missing `gh` login fails
  the calls that need it.
- **Merge settings** with `--skip-doctor`. A refusal opens a
  `merge-settings` card.
- **Base fetch.** It sends the integration-behind and local-base-behind
  warnings to the orchestrator as notices.
- **`doctor --predict`** (#408) and **the background watchers**.

Each of these is bounded by a 2 minute timeout. A doctor that hangs records
a `doctor_timeout` event.

### The guard

`TestJourneyStartupFirstFrameNeverWaitsOnGitHub`
(`internal/e2e/journeys_startup_test.go`) starts `saddle up` with the doctor
on while the fake GitHub holds every `gh` call unanswered (`fakegh.State.Hold`).
It reads the first frame's time from saddle's own trace file and fails if it
is above the budget: 8s by default, or `E2E_STARTUP_BUDGET`. It measured
about 0.26s at load 18. The budget is wide because the hold already makes
the test fail outright if anything before the first frame calls GitHub. Put
the full doctor back before the TUI and it fails at about 10.2s. It then
releases GitHub and waits for the full doctor to finish after the frame. It
polls on conditions and never sleeps.

Unit tests: `TestRunLocalSkipsTheSlowChecks` (doctor),
`TestFirstFrameFiresOnceOnTheFirstLaidOutView` (tui),
`TestOrchestratorLeavesTheBaseFetchForLater` (app), and the
`TestUpDeferred*`, `TestStartupTrace*` and `TestLazyStart*` tests in
`internal/cli`.

### Follow-ups

- `GCCounts` takes 23 to 25s on this repo under load. It is off the startup
  path now, but `saddle doctor` and `saddle down` still wait on it.
