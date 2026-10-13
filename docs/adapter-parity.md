# Adapter parity: claude, codex, grok

Saddle grew up on Claude Code. This page lists every feature that assumed
it, how codex and grok behave there, and what was fixed or filed (#480).
Gemini is listed where it shares codex's hookless path.

| Feature | claude | grok | codex |
|---|---|---|---|
| Saddle hooks (claims enforced, notices with tool calls) | yes | yes (project hooks, #181) | no: claims advisory, notices typed in |
| Idle / running status | Stop hook | Stop hook | screen watcher (fixed) |
| Composer read (`tmux.ReadInput`): draft guard, Enter retry | `❯` / `>` | `>` / `❯` assumed, unverified (#488) | `›` (fixed) |
| Approval and trust prompts (`DetectPrompt`) | yes | bypassPermissions, wording unverified (#488) | approval and trust screens (fixed) |
| Unattended permission mode | `permission_mode` | `[grok] permission_mode` | mapped from `permission_mode` (fixed) |
| Quota banner (`DetectLimitBanner`) | yes | xAI credit and spend-cap errors (fixed) | absolute, dated and relative resets (fixed) |
| `out_of_quota_until` | banner reset | banner reset, else 1h | banner reset, including "in 2 hours" (fixed) |
| After the quota reset | continues on its own | saddle types a continue line (fixed) | saddle types a continue line (fixed) |
| Out of quota mid-task (#481) | handed to the next adapter in its worktree, or parks (`[adapters] failover`) | same | same |
| Orchestrator out of quota (#481) | TUI moves it to grok with a state summary | TUI moves it to claude | n/a: no headless session |
| Orchestrator context meter | transcript | usage.json snapshot | n/a: `saddle up codex` runs the orchestrator on Claude Code |
| Auto-compact | `/compact <keep>` | none: told to summarize (#488) | `/compact` |
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

## Filed

- #488: verify the codex and grok screen detectors (composer, prompts,
  quota errors, working indicator) against captured panes, and find out
  whether the grok CLI has a compact command.
- #489: enforce claims for hookless adapters (codex, gemini) without the
  agent's cooperation, e.g. a per-worktree pre-commit hook with an escape
  hatch.

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
