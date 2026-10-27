# Adapter parity: claude, codex, grok, opencode

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

opencode (epic #508) runs workers and, since #745, a headless orchestrator.
Where it differs from codex, the hookless worker it otherwise resembles:

| Feature | opencode |
|---|---|
| Saddle hooks | no (`Hooks()` is false), but a claims plugin enforces claims on every write (#532); pre-commit and `done` checks stay on as a backstop |
| Idle / running status | screen watcher; working indicator `esc interrupt` |
| Approval prompts | `Allow once / Allow always / Reject` row, Enter allows once |
| Unattended permission mode | `acceptEdits` and `default` add nothing, `plan` is `--agent plan`, anything else `--auto` |
| Quota | screen panes (retry waits count) and the `opencode.db` error rows, per provider/model (#531, #573) |
| Usage meter | `opencode.db` sessions run in the worktree (#533) |
| Model in spawn and status | `[adapters.opencode] model`, `provider/model` |
| Orchestrator (headless) | opencode bridge: one `opencode run --format json` turn per message, resumed with `-s` (#745) |
| Orchestrator out of quota | the run's error event; limit wordings are labelled so failover reads them (#745) |
| Orchestrator context meter | the bridge's ledger of each model call's tokens (#745) |
| Orchestrator deny list | opencode's `permission` config: edits, subagents and history-moving git denied (#745) |

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
  tool call, so claims can be enforced like claude's and grok's: see the
  claims plugin (#532) below.
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

Landed since this spike (#520, #531, #532, #533, #573): the screen
detectors (`DetectPrompt` reads the `Allow once / Allow always / Reject` row,
the hookless working indicator list includes opencode's `esc interrupt`,
`OpenCodeScreenLimit` reads the quota panes), the claims plugin, usage
metering and the limit handling, each in its own section below.
opencode is in `config.AdapterNames` (#520) and the default
`[adapters] order`; it is not in the orchestrator brief's provider list
(`Providers`). It has a headless orchestrator since #745 (below). Tests:
`TestOpenCodeLaunchRunsFakeBinary`, `TestOpenCodeLaunchWithoutModel`,
`TestOpenCodeApprovalFlags`, `TestOpenCodeAvailability`.

## opencode claims plugin (#532)

Launch writes `saddle-claims.mjs` into the run dir and the run-dir
`opencode.json` loads it (`plugin: ["file://…/saddle-claims.mjs"]`), so
nothing lands in the repo. Its `tool.execute.before` sends each `write`,
`edit`, `multiedit`, `apply_patch`, `patch` and `bash` call to `saddle hook`
as a PreToolUse payload: opencode's tool name, its args as `tool_input`, and
the session directory as `cwd`. That is Claude's hook matcher; reads never
reach saddle. When saddle denies, the plugin throws, and opencode fails the
call with saddle's reason, which the model reads. The hook resolves a
relative `filePath` against `cwd` and checks every file an `apply_patch`
patch adds, updates, deletes or moves to. As with claude and grok, an
unclaimed file becomes the task's, and `git commit --no-verify` is denied. If
saddle can't run or errors, the call goes through (the hook fails open), and
the commit check below still catches it.

Verified with opencode 1.19.0 and `opencode/big-pickle` in a scratch repo:
a write to a file another task claimed failed with the write-guard reason
naming the owner, a new file was written, edited and claimed for the task,
and `git commit --no-verify` was refused. opencode resolves its project from
`$PWD`, which launch.sh's `cd` sets.

`Hooks()` stays false. In saddle it means more than claims: the session id
comes from SessionStart, notices arrive as hook context after a wake line,
the Stop hook marks the agent idle, and commits skip the claims check. The
plugin does none of these, so opencode still gets its notices typed in, the
screen watcher marks it idle, and the pre-commit and done checks (#489) stay
on as a backstop. Status shows `hooks: false`. Tests:
`TestOpenCodeLaunchWritesClaimsPlugin`, `TestOpenCodeClaimsPluginHarness`
(runs the generated plugin under bun or node against a fake saddle; skipped
when neither is installed), `TestOpenCodePluginPayloadEnforcesClaims`,
`TestPatchPaths`.

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

## opencode usage and limits (#533)

Usage and quota for opencode are wired in (#531 wrote the reader and
detectors):

- `OpenCode.Usage()` returns `usage.OpenCodeDBPath` (`$XDG_DATA_HOME`, default
  `~/.local/share`). The usage meter's collector reads that database with
  `usage.ReadOpenCode`, limited to sessions run in the task's worktree, so USAGE
  shows opencode per provider/model.
- The idle watcher (`ObserveHooklessScreen`) runs `OpenCodeScreenLimit` on the
  worker's screen and, once the screen has sat still, `OpenCodeDBLimit`. A hit
  calls `MarkModelExhausted` for the worker's *configured* provider/model, not
  the footer's display name, so `[fallback]` and spawn failover skip it.
  `config.AdapterNames` already lists opencode.

The reader depends on the 1.19.0 database layout and skips rows it cannot
parse. Tests: `TestCollectorReadsOpenCodeDB`, `TestUsageMeterReadsOpenCodeDB`,
`TestOpenCodeLimitScreenMarksConfiguredModelAndFailsOver`.

## opencode limits, captured (#573)

The detectors were checked against what opencode 1.19.0 really shows and
stores when a provider refuses it for a limit. No account was run dry: a
local stand-in (`internal/usage/testdata/opencode/limitmock.py`) answered
opencode with the exact status, body and `retry-after` OpenCode Zen sends,
taken from Zen's server source, and with OpenAI's `insufficient_quota` 429.
What opencode did with each response is its own. Fixtures are in
`internal/agent/testdata/opencode/limit-*.txt` (panes) and
`internal/usage/testdata/opencode/limits.jsonl` (rows); their READMEs say how
each was made.

What differed from the synthetic fixtures, and is now handled:

- **Zen limits are retried, not failed.** opencode treats Zen's 429
  `FreeUsageLimitError`/`GoUsageLimitError` as retryable and waits out
  `retry-after` (the time to the reset) up to five times. The turn stays
  running, with a dialog and a status line:
  `Free usage exceeded, subscribe to Go [retrying in 9h 29m attempt #1]` or
  `5 hour usage limit reached. It will reset in 2 hours 5 minutes. …`.
  Nothing reaches the database until the retries run out (it keeps no retry
  state), so the screen is what catches these. With no reset in the words,
  a `[retrying in …]` wait longer than `OpenCodeExhaustFor` is taken as the
  reset; a shorter one may be a backoff and keeps the default hour.
- **Zen's own wording.** The free tier's stored message is a bare
  "Rate limit exceeded. Please try again later.", so the database reader
  also checks the response body's error type (Zen's `*LimitError`,
  `CreditsError`, `MonthlyLimitError`, `UserLimitError`, OpenAI's
  `insufficient_quota`), as opencode does. Go's message says
  "Resets in 2hr 5min", which is now parsed, and the reader falls back to
  the stored `retry-after`.
- **Billing limits.** Zen answers an empty balance or a spending limit with
  a 401 that opencode does not retry: "Insufficient balance…", "…monthly
  spending limit of $20…". These are now limits too.
- opencode's spinner draws `⬝` over the status line's spaces; the screen
  parser reads it as a space.

**Model.** opencode stores the failing turn's `providerID`/`modelID`
(`opencode`/`big-pickle`, `openai`/`gpt-4o-mini`), the same `provider/model`
string a worker is launched with (`-m`). The screen shows a display name
("Big Pickle OpenCode") instead, which is why the screen detector marks the
worker's configured model.

Still untested against a live account: whether Zen's production wording
matches its source at the time of capture (the console source used was
current on 2026-10-08), and other providers' limit shapes (Anthropic,
Google) beyond OpenAI's. Tests: `TestOpenCodeScreenLimitCaptured`,
`TestOpenCodeDBLimitCaptured`, `TestParseOpenCodeLimitCaptured`,
`TestReadOpenCodeLimitsCaptured`.

## opencode headless orchestrator (#745)

The #508 assessment (`opencode run --format json` gives resumable one-turn
JSONL; MCP reaches it through `OPENCODE_CONFIG`) held, so
`[orchestrator] adapter = "opencode"` now works. Like codex, the TUI's
orchestrator runs through a bridge, `saddle opencode-bridge`, which runs one
`opencode run --format json [-m provider/model] [-s <id>] <args>` turn per
user message, with the message on stdin (opencode appends piped stdin to the
message, which keeps a long brief off the command line). The loop it shares
with the codex bridge (`turnBridge`) keeps the session across turns, opens
each new session with the brief, and starts fresh once when a stored id
won't resume (`-s` on an unknown id prints "Session not found" and no JSON).

- **Events.** As captured from opencode 1.19.0: every event carries
  `sessionID`, which becomes the init line; `text` parts are assistant text;
  `tool_use` parts are tool calls, with opencode's tools named as Claude's
  (`bash` is `Bash`) and saddle's MCP tools (`saddle_status`) as
  `mcp__saddle__status`; `step_finish` carries one model call's tokens;
  `error` is the turn's failure. run prints no turn-end event, so the
  bridge writes the result line, with the last step's usage, when the
  process exits cleanly.
- **MCP.** The run-dir `opencode.json` that `OPENCODE_CONFIG` names carries
  the saddle server with the orchestrator's environment, as for workers.
  `[adapters.opencode] cmd` and model apply; of its args, only `--agent`,
  `--variant`, `--log-level`, `--pure` and `--auto` reach run.
- **Permissions.** The permission mode maps as for workers (`--auto` unless
  `acceptEdits`, `default` or `plan`). The orchestrator's deny list becomes
  opencode's `permission` config: `edit` (write, edit and patch) and `task`
  (subagents) denied, and each `Bash(git push:*)` style rule a denied
  command pattern. Verified: under `--auto` the model had no write tool, and
  `git checkout -b` failed with the rule. Claude-only tools have no
  counterpart. There are no hooks: `[saddle]` events arrive between turns.
- **Quota.** A failed turn's `error.data.message` (or the stderr of a run
  that printed no JSON) is the error result. When it is an opencode limit
  (`usage.OpenCodeErrorLimit`: the wordings, or a quota type in the
  response body, such as Zen's free tier's bare "Rate limit exceeded")
  that `DetectLimitBanner` would miss, it is labelled "opencode quota
  exceeded:" with the reset, so the TUI's failover moves the orchestrator.
  opencode is in `OrchestratorAdapters`, so it is also a failover target.
- **Compaction.** `opencode run --command compact` fails with "Unexpected
  server error", so `/compact` is the codex emulation: a summary turn on the
  old session, a fresh session opened with the brief and the summary, and a
  `compact_boundary`.
- **Context meter.** The bridge appends each `step_finish` to
  `opencode-context.jsonl` in the run dir as a Claude transcript line
  (input, cache read and cache write are the prompt), which the compact
  watcher tails. A new session clears it. The window comes from the model
  table; models it doesn't know (`big-pickle`) count as 200K.
- **Doctor.** The orchestrator row checks `opencode --version`, that
  `opencode run --help` offers `--format`, and the login, warning
  "opencode is not logged in: run opencode providers login" without one.

Verified by hand with opencode 1.19.0 and `opencode/big-pickle`: a saddle
`status` call over MCP, a resumed turn recalling the last one, `/compact`
moving to a fresh session, and a denied `git checkout`.

Not covered: opencode retries Zen's 429 limits for up to their reset (#573),
and `run` prints nothing while it waits, so such a turn sits busy until the
retries run out rather than failing over. `saddle init` doesn't pick an
opencode orchestrator on a machine with only opencode, since opencode is not
a worker harness. Tests: `TestOpenCodeBridgeResumesAcrossTurns`,
`TestOpenCodeBridgeStartsFreshWhenSessionMissing`,
`TestOpenCodeBridgeReportsQuotaError`, `TestOpenCodeQuotaText`,
`TestOpenCodeBridgeCompactStartsFreshSessionFromSummary`,
`TestOpenCodeBridgeWritesContextLedger`,
`TestOpenCodeContextLedgerFollowsEachTurn`, `TestHeadlessOpenCodeWritesSpec`,
`TestCodexBridgeFreshRetryCarriesBrief`, `TestOrchestratorRunsOnOpenCode`,
`TestFailOverOrchestratorToOpenCode`, `TestOpenCodeOrchestratorCheck`,
`TestOrchestratorGoldens/opencode-orchestrator`, `TestRunTurns`
(fakeopencode) and `TestJourneyOpenCodeOrchestrator`.

## opencode e2e

`TestJourneyOpenCodeOrchestrator` (`internal/e2e`) runs saddle up with
`[orchestrator] adapter = "opencode"` against `fakeopencode`: doctor warns
of the missing login, then is clean; a turn calls saddle's `status` over the
MCP server in `OPENCODE_CONFIG`; the next turn resumes the session; a
`[saddle]` event arrives as a turn; past `compact_at` it is compacted into a
fresh session; and a quota error moves it to claude.

`TestJourneyOpenCodeWorker` (`internal/e2e`) drives a fake `opencode` (no
real binary or login): a spawn without a login is refused by name with
"opencode providers login" and leaves no task; with an `auth.json` status
lists opencode available, the spawn starts the TUI with `--prompt`, and the
run-dir `OPENCODE_CONFIG` carries the saddle MCP server and the claims
plugin, with nothing written into the worktree.
