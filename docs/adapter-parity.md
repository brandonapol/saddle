# Adapter parity: claude, codex, grok

Saddle grew up on Claude Code. This page lists every feature that assumed
it, how codex and grok behave there, and what was fixed or filed (#480).
Gemini is listed where it shares codex's hookless path. opencode, the
fourth adapter (epic #508), has its own section below.

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
| Orchestrator out of quota (#481) | TUI moves it to the next of grok and codex with quota, with a state summary | TUI moves it to claude, then codex | codex bridge: one `codex exec` turn per message, resumed (#506) |
| Orchestrator context meter | transcript | usage.json snapshot | newest rollout started in the repo root (#506); `saddle up codex` still starts the orchestrator on Claude Code |
| Working indicator (hookless idle) | n/a: Stop hook | `Ctrl+c:cancel` footer (#488) | `esc to interrupt`, verified (#488) |
| Auto-compact | `/compact <keep>` | `/compact`, also headless (#488) | `/compact`; headless: a summary turn, then a fresh session opened with it (#506) |
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
- **Grok approval input (#496).** On grok's approval screen there is no
  `❯` composer, so `ReadInput` read empty and `Deliver` and the done-orphan
  nudge would type into the menu. `ReadInput` now returns `prompt` for
  grok's radio options and for a numbered menu behind `❯` or `›`, and
  `Deliver` never types into it, even forced. Notice delivery also holds
  back while `DetectPrompt` sees a prompt; the orphan nudge accepts a
  trusted checkout's trust prompt or marks the task needs-you instead of
  typing. Tests: `TestReadInputApprovalTail`, `TestNotDeliveredIntoApproval`,
  `TestDeliverWaitsWhileBlocked`, `TestNotifyNotTypedIntoPromptScreen`,
  `TestDoneOrphanNudgeLeavesApprovalScreen`.

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

## Codex headless orchestrator (#506)

Codex has no persistent stdin protocol, so the TUI's orchestrator runs on
codex through saddle's `codex-bridge`, parallel to grok's `grok-bridge`.
For codex each user message is one `codex exec --json` turn. The first turn
starts a session; later turns
run `codex exec resume <id>`, so context carries across turns. Codex's JSONL
events become the stream-json the TUI parses: `thread.started` is the init
line, agent messages are assistant text, commands, MCP calls, file changes
and searches are tool calls, and `turn.completed` or `turn.failed` is the
result, so a usage-limit message reaches failover. A session id that won't
resume (another adapter's, a deleted rollout) is retried once fresh.

- **MCP.** The saddle server is passed with `-c mcp_servers.saddle.*`
  overrides on every turn, as interactive codex workers get it. Verified
  with codex 0.154.0: `exec` and `exec resume` load it and call its tools.
- **Brief.** Codex has no system-prompt flag, so the brief opens each new
  session's first message.
- **Sandbox.** `exec resume` takes no `--sandbox`, so the permission mode
  maps to `-c sandbox_mode=…` (`acceptEdits` is workspace-write, `plan` and
  `default` read-only) or, by default,
  `--dangerously-bypass-approvals-and-sandbox`: the orchestrator runs saddle
  and git in the main checkout. Of `[adapters.codex] args`, only `-c`,
  `--enable` and `--disable` reach exec; interactive-only flags would fail
  every turn.
- **Compaction.** `exec` has no `/compact`. The bridge reads a `/compact`
  line, asks the session for a summary, opens a fresh session with the brief
  and that summary, and reports a `compact_boundary`.
- **Limits.** Codex has no hooks and no tool deny list, so the orchestrator
  deny list does not apply on codex. Interrupts end the bridge with SIGINT,
  as grok's do.

Tests: `TestCodexBridgeResumesAcrossTurns`,
`TestCodexBridgeStartsFreshWhenResumeFails`,
`TestCodexBridgeReportsQuotaError`,
`TestCodexBridgeCompactStartsFreshSessionFromSummary`, `TestCodexExecArgs`,
`TestHeadlessCodexWritesSpec`, `TestFailOverOrchestratorToCodex`,
`TestFailOverOrchestrator`.

## opencode: spike and skeleton (#509)

opencode 1.19.0 (`opencode` on PATH, `[adapters.opencode] cmd` to override)
was run in tmux against a scratch repo. Findings:

- **Launch.** `opencode [project]` is the TUI. `--prompt <text>` fills the
  composer and submits it, so the first message needs no typing.
  `-m/--model provider/model` picks the model; with none, opencode uses its
  configured default. The adapter's launch.sh `cd`s into the worktree and
  runs `opencode [--model M] [--auto] [args] --prompt "$(cat prompt.md)"`.
  Typed text plus Enter submits, so hookless notices are typed in whole on
  one line, as for codex.
- **Permissions.** opencode allows edits and shell commands by default and
  asks only for what its `permission` config marks `ask`. `--auto`
  approves those too. The permission mode maps like codex's: `acceptEdits`
  and `default` add nothing, `plan` is `--agent plan` (opencode's read-only
  agent), anything else is `--auto`. The composer then reads `Build auto`.
- **MCP.** `OPENCODE_CONFIG=<file>` is merged over the user's global and
  project config, so the adapter writes `opencode.json` in the run dir with
  `mcp.saddle = {type: "local", command: [saddle, "mcp"], environment:
  {SADDLE_ROOT, SADDLE_TASK}}` and nothing lands in the repo.
  `opencode mcp list` shows it connected, and the worker calls it as
  `saddle_status` and so on. A user's own `OPENCODE_CONFIG` is replaced for
  the worker. `OPENCODE_DISABLE_AUTOUPDATE=1` is set too.
- **Brief.** It opens the first message, with the hookless claim rules, as
  for codex and gemini. opencode's `instructions` config could carry it in
  the system prompt instead.
- **Hooks.** opencode has JS/TS plugins (`.opencode/plugin/`, or `plugin`
  in config) with `tool.execute.before`/`after` and session events such as
  `session.idle`. A plugin that throws in `tool.execute.before` blocks the
  tool call, so claims could be enforced like claude's and grok's. Saddle
  installs no plugin yet, so `Hooks()` is false: claims are checked at
  commit and done (#489) and status shows `hooks: false`.
- **Login.** Status shows opencode ok when it is on PATH and has a
  credential: `auth.json` under `$XDG_DATA_HOME/opencode` (default
  `~/.local/share/opencode`), written by `opencode providers login`, or a
  provider key in the environment (`OPENCODE_API_KEY`, `ANTHROPIC_API_KEY`,
  `OPENAI_API_KEY`, `OPENROUTER_API_KEY` and others). Otherwise the reason
  is "opencode is not logged in: run opencode providers login". opencode's
  free `opencode/*` models answer with no login at all. Saddle still asks
  for one, because those models can't be metered or failed over from yet.
- **Screens** (`internal/agent/testdata/opencode`). opencode runs in the
  alternate screen, which `capture-pane` reads. Busy: `esc interrupt` in
  the footer. Idle: the footer shows the cwd and context use
  (`17.0K (8%)`). Approval: a `△ Permission required` panel with `Allow
  once   Allow always   Reject` (Enter allows once). The composer is a `┃`
  box with the agent and model under it, and no placeholder. Quota: the
  binary's wordings are "Free usage exceeded, subscribe to Go", "<plan>
  usage limit reached. It will reset in 2 hours 5 minutes…" and "Quota
  exceeded. Check your plan and billing details.". Retries show
  `[retrying in <n> attempt #<k>]`. The quota pane is assembled from those
  strings, because a limit can't be hit on demand.
- **Manual run.** The adapter's launch.sh, with `opencode/big-pickle`, wrote
  a file, committed it through saddle's pre-commit hook, called
  `saddle_status` and went idle.

Not yet (later tickets in #508): screen detectors for these panes
(`DetectPrompt`, `ReadInput`, working indicator, `DetectLimitBanner`), a
claims plugin, usage metering (`opencode stats`, or its `opencode.db`
sessions), and a headless orchestrator (`opencode run` or `opencode
serve`). Until the detectors land, the hookless idle watcher's working indicator
(`esc to interrupt`, `esc to cancel`, `Ctrl+c:cancel`) never matches
opencode's `esc interrupt`; a busy opencode reads as running only while its
animated footer keeps the screen changing.
opencode is not in `config.AdapterNames`, so `[adapters] order` and
`[fallback]` can't name it yet, and it is not in the orchestrator's
rotation (`Providers`). Tests: `TestOpenCodeLaunchRunsFakeBinary`,
`TestOpenCodeLaunchWithoutModel`, `TestOpenCodeApprovalFlags`,
`TestOpenCodeAvailability`.

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
