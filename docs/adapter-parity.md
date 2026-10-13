# Adapter parity: claude, codex, grok

Saddle grew up on Claude Code. This page lists every feature that assumed
it, how codex and grok behave there, and what was fixed or filed (#480).
Gemini is listed where it shares codex's hookless path.

| Feature | claude | grok | codex |
|---|---|---|---|
| Saddle hooks (claims enforced, notices with tool calls) | yes | yes (project hooks, #181) | no: claims checked at commit and done (#489), notices typed in |
| Idle / running status | Stop hook | Stop hook | screen watcher (fixed) |
| Composer read (`tmux.ReadInput`): draft guard, Enter retry | `❯` / `>` | `│ ❯ │` box, verified (#488) | `›`, placeholder in plain captures (#488) |
| Approval and trust prompts (`DetectPrompt`) | yes | `1 (●) Yes` approval screen (#488) | approval and trust screens, verified (#488) |
| Unattended permission mode | `permission_mode` | `[grok] permission_mode` | mapped from `permission_mode` (fixed) |
| Quota banner (`DetectLimitBanner`) | yes | weekly, free-tier, credit and spend-cap errors (#488) | usage, spend-cap and workspace-credit errors; absolute, dated and relative resets (#488) |
| `out_of_quota_until` | banner reset | banner reset, else 1h | banner reset, including "in 2 hours" (fixed) |
| After the quota reset | continues on its own | saddle types a continue line (fixed) | saddle types a continue line (fixed) |
| Out of quota mid-task (#481) | handed to the next adapter in its worktree, or parks (`[adapters] failover`) | same | same |
| Orchestrator out of quota (#481) | TUI moves it to grok with a state summary | TUI moves it to claude | n/a: no headless session |
| Orchestrator context meter | transcript | usage.json snapshot | n/a: `saddle up codex` runs the orchestrator on Claude Code |
| Working indicator (hookless idle) | n/a: Stop hook | `Ctrl+c:cancel` footer (#488) | `esc to interrupt`, verified (#488) |
| Auto-compact | `/compact <keep>` | `/compact`, also headless (#488) | `/compact` |
| Hung gates, orphaned done | adapter-agnostic | same | same; the orphan nudge types notices in whole |
| Model in spawn and status | `[claude] model` | `[grok] model` | `[adapters.codex] model` (fixed) |
| Adapter in status | `adapter` on each task (fixed) | same | same |

## Fixed in #480

- **Codex composer.** `tmux.ReadInput` only knew `❯` and `>`. Codex draws
  `›`, so saddle saw every codex input box as empty: it typed over an owner's
  draft and never pressed a lost Enter again (#467). It now reads `›` too.
  Test: `TestReadInputCodexComposer`.
- **Codex prompts.** `DetectPrompt` missed codex's approval screen ("Would
  you like to run the following command?", `› 1.` menus, "Press enter to
  confirm") and its trust screen. They now count as asks. The trust screen
  is an ask rather than `PromptTrust`, because the Down, Enter that accepts
  Claude's dialog picks "No" in codex's. Test: `TestDetectPromptCodex`.
- **Codex permission mode.** Codex launched with no approval flags, so an
  unattended worker sat on its trust screen and asked approval for commands.
  The permission mode now maps to codex flags the way gemini's does:
  `acceptEdits` is `--full-auto`, `plan` and `default` add nothing, and
  anything else is `--dangerously-bypass-approvals-and-sandbox`. Codex's
  workspace-write sandbox can't commit to a worktree whose git dir is
  outside it. `[adapters.codex] args` that set approval or sandbox replace
  saddle's flags. Test: `TestCodexApprovalFlags`.
- **Quota wording.** Codex says "try again in 2 hours 5 minutes" or "try
  again at Oct 14th, 2026 3:04 PM". The first gave no reset, so codex
  stayed out for the 1h fallback. The second lost its date. xAI says "used
  all available credits or reached its monthly spending limit", which wasn't
  detected at all. All three now parse, and a relative reset shows as a
  clock time in `out_of_quota_until`. Tests: `TestDetectLimitBanner`,
  `TestParseReset`, `TestCodexRelativeResetSetsOutOfQuotaUntil`.
- **Resuming after the reset.** The engine told the orchestrator that a
  parked worker "continues on its own", and both the engine and the TUI
  then waited for the banner to clear. Claude Code does continue on its
  own. Codex and grok end the turn on the error and wait for input, so they
  stayed parked for good. Now, once the adapter's quota is back
  (`ExhaustedAdapters` drops it), saddle types `LimitResumeLine` into the
  worker, once per banner. That banner left in scrollback is not read as a
  new limit (`ResumedPast`). The park and rotation notices now say which
  way each adapter recovers. Tests: `TestEngineResumesCodexAfterItsReset`,
  `TestWatchScreensResumesCodexAfterReset`.
- **Idle without a Stop hook.** Only the Stop hook ever marked a worker
  idle. A codex worker that stopped without calling done stayed "running",
  and the orchestrator never heard "stopped without calling done". The
  engine and the TUI now pass hookless workers' screens to
  `ObserveHooklessScreen`. A screen that sits still for `HooklessIdleAfter`
  (20s) with no working indicator ("esc to interrupt") and no prompt marks
  the worker idle. The working indicator coming back marks it running.
  Hooked workers are left to their hooks. Tests:
  `TestEngineMarksHooklessWorkerIdleFromItsScreen`,
  `TestWatchScreensMarksCodexIdle`.
- **Model and adapter in status.** Codex had no configured model, so its
  tasks showed none. `[adapters.<name>] model` now sets codex's and
  gemini's default, and status's task view names each task's `adapter`.
  Tests: `TestAdapterModelFromConfig`, `TestAdaptersAndSpawnCaps` (config),
  `TestStatusNamesEachTasksAdapter`.

## Verified against captured panes (#488)

Codex 0.154.0 and Grok 1.0.50 were run in tmux and captured in each state
(`internal/tmux/testdata`, `internal/app/testdata`, `internal/usage/testdata`).
Quota errors can't be produced on demand, so their wordings come from the
CLIs' binaries, placed where each CLI prints a turn's error.

- **Codex placeholder in a plain capture.** "Ask Codex to do anything" (or
  "Ask a follow-up question") is drawn faint, but without SGR escapes
  `ReadInput` read it as a draft, so `Deliver` waited on an idle codex. Both
  wordings now read as a ghost. Test: `TestReadInputCodexGrokFixtures`.
- **Grok approval screen.** Grok draws options as `┃  1 (●) Yes, proceed`
  with no menu cursor and none of the others' wordings, so `DetectPrompt`
  saw nothing and remote prompts had no options. Both read it now. Tests:
  `TestDetectPromptCapturedPanes`, `TestParsePromptGrokApproval`.
- **Grok working indicator.** A turn in progress shows `Ctrl+c:cancel` in
  the footer, not "esc to interrupt". Grok workers have a Stop hook, so
  this only matters if one runs hookless. Test:
  `TestWorkingIndicatorCapturedPanes`.
- **Quota wording.** Codex says "You hit your spend cap…" and "You've
  reached your workspace credit limit"; grok says "You hit your weekly
  limit." and "You hit your free usage limit." None were detected. Codex's
  idle "You have 1 usage limit reset available" line was read as the reset
  time ("available. Run /usage…"), beating the real one; only "resets",
  "reset at" and "try again" count now. A reset wrapped onto the next line
  was lost because the tail was joined bottom-up. Test:
  `TestDetectLimitBannerCapturedPanes`.
- **Grok compact.** Grok has `/compact` ("takes no arguments"), and it
  compacts a headless `--prompt-file` turn, which is how the TUI runs a grok
  orchestrator. `CompactCommand` sends it. Tests:
  `TestCompactCommandPerHarness`, `TestCompactInjectsHarnessCommand`.
- **Still open.** On grok's approval screen there is no `❯` composer, so
  `ReadInput` reads empty. The notice wake checks `DetectPrompt` first;
  `Deliver` and the orphan nudge don't. Workers run `bypassPermissions`,
  so the screen is rare.

## Fixed in #489

- **Hookless claims.** Codex and gemini claims were advisory: saddle
  only listed the claimed files a branch touched in the done notice. Now
  hookless launches export `SADDLE_HOOKLESS=1`, and the pre-commit hook
  `saddle init` installs (chaining any repo pre-commit) refuses a commit of
  a file another task claims, naming the owner, and claims unowned files for
  the committer. Done refuses a branch that still changes another task's
  files, which catches `--no-verify`. `SADDLE_ALLOW_CLAIMED=1` overrides
  both and is recorded. Claude and grok commits skip the check. Tests:
  `TestCheckCommitEnforcesClaimsForHooklessTask`,
  `TestCheckCommitLeavesHookedTasksAlone`,
  `TestDoneRefusesHooklessBranchTouchingClaimedFile`,
  `TestDoneAllowClaimedOverridesAndTellsOrchestrator`,
  `TestPreCommitRunsOnlyForHooklessAgents`.

## Already at parity

- Hung-gate watchdog (`HungGates`) looks at processes, not agents.
- The done-orphan nudge (`nudgeOrphan`) types a hookless agent's notices in
  whole and checks the submit.
- The `send_keys` and `message` tools go through `tmux.Submit`, which now
  reads codex's composer.
- Usage metering parses codex rollouts and grok usage snapshots (#478).
- Spawn refuses an unavailable adapter by name and never substitutes one
  (#182). Rotation warns when claimed work moves to a hookless adapter
  (#321).
