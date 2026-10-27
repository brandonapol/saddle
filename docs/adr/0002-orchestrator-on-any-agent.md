# ADR 0002: The orchestrator runs on codex or grok, not only Claude Code

- Status: Accepted
- Issue: #668 (this change); builds on #150 (saddle up <agent>), #481 and #506 (orchestrator failover to grok and codex), #646 (worker harness fallback), #637 (codex pricing)

## Context

`saddle up` runs the orchestrator headless in the TUI. Which CLI it runs on
was decided in one place, `App.orchDefault`: grok-bridge under
`harness = "grok"`, Claude Code otherwise. Codex was only reachable through
a quota failover (#506), which already built most of what a codex
orchestrator needs:

- `internal/agent/codex_bridge.go` replays each user message as one
  `codex exec --json [resume <id>] -` turn and translates codex's JSONL
  events into the stream-json the TUI parses. The saddle MCP server is
  passed on every turn with `-c mcp_servers.saddle.*` overrides, the brief
  opens each new session (codex has no system-prompt flag), and `/compact`
  is emulated: the old session writes a summary and a fresh one starts from
  it.
- `OrchIdentity` already renders a brief, header and compact hint for a
  codex session, and `OrchestratorContext` reads a codex rollout.

What was missing: no config chose codex as the orchestrator's default, so
with only Codex installed `saddle up` fell back to codex for workers (#646)
but still started Claude Code for the orchestrator, and `saddle doctor`
reported claude, the orchestrator allowlist and orchestrator skills as
problems that had nothing to do with the run.

## Decision

### 1. Config: `[orchestrator] adapter` and `model`

```toml
[orchestrator]
adapter = "codex"   # "claude", "codex" or "grok"; empty picks (below)
model   = "gpt-5-codex"  # optional; empty is the adapter's own default
```

`config.Load` rejects an adapter outside `claude`, `codex`, `grok`
(the adapters with a headless session; gemini and opencode have none).

The orchestrator's adapter is resolved by `agent.OrchestratorAdapter`, one
rule shared by `saddle up`, `saddle init` and `saddle doctor`:

1. `[orchestrator] adapter` when set. It is honoured even when that CLI
   can't run: the owner asked for it, and doctor says what is wrong.
2. Else the harness's own CLI under `harness = "grok"` (unchanged).
3. Else Claude Code when it is installed (unchanged for every existing
   setup, including `harness = "codex"` with claude installed).
4. Else the first of the harness, then `[adapters] order`, that can run
   and has a headless session: the same rule as the worker fallback (#646).
5. Else Claude Code, so the failure names the CLI people expect.

Model: `[orchestrator] model`, else the adapter's existing key
(`[claude] orchestrator_model`, `[grok] orchestrator_model`,
`[adapters.codex] model`).

A quota failover (`.saddle/orchestrator-adapter`) still wins over all of
this for the run it happens in, as before.

`saddle init` writes `[orchestrator] adapter = "<fallback>"` when Claude
Code can't run and another headless agent can, unless the owner already set
it; this mirrors how it writes `harness`.

### 2. How events reach a codex orchestrator

Claude Code gets `[saddle]` notices two ways: the TUI types queued events
into the session when it is idle, and saddle's hooks (`PostToolUse`,
`UserPromptSubmit`, `Stop`) attach notices to tool calls mid-turn.

Codex has no hooks, so a codex orchestrator gets **injected input only**:
the TUI's `deliver` loop, which already runs for every headless
orchestrator, takes the orchestrator's notices from the store when the
bridge reports the turn finished, and sends them as the next user message
(one `codex exec resume` turn). No notice file is polled; the store is the
queue, so nothing is lost between turns, and the notice waker keeps
working as it does for Claude.

What is lost:

- **No mid-turn delivery.** An event that happens during a long codex turn
  waits for the turn to end. Claude sees it on its next tool call. Turns of
  an orchestrator are short (it reads status and calls tools), so the delay
  is seconds to a few minutes.
- **No claim enforcement on the orchestrator's own writes.** Not needed:
  the orchestrator writes no code. Its brief says so (`OrchIdentity.Brief`
  for a hookless session), and Claude's `OrchestratorDeny` list (no
  Edit/Write, no `git push`, `merge`, `reset`, `checkout`) has no codex
  equivalent. The merge train's ref guard (pre-push hook, #220) still
  refuses pushes to saddle's branches from any process that isn't the train,
  so a stray `git push` from a codex orchestrator is denied by saddle's
  guard, not by a tool list.
- **No permission allowlist.** Codex exec does not prompt; the bridge runs
  it with `--dangerously-bypass-approvals-and-sandbox` (the orchestrator
  runs `saddle`, `gh` and git against the main checkout and needs network)
  unless the owner chose a sandbox in `[adapters.codex] args` or a
  restrictive `[claude] permission_mode`. Doctor warns when that sandbox is
  read-only, since `gh` and saddle's state writes then fail.
- **No skills or slash commands** besides saddle's emulated `/compact`.

### 3. Grok

Grok already had a headless orchestrator (grok-bridge, under
`harness = "grok"`). `[orchestrator] adapter = "grok"` now selects it
regardless of the harness, at no extra cost. Grok's hooks run in its
session, so it keeps mid-turn notices.

### 4. Doctor

A new `orchestrator` check names the adapter and model the orchestrator
will run on and checks that adapter's own pieces:

- **claude**: the existing `claude`, `orchestrator allowlist` and
  `orchestrator skills` checks cover it.
- **codex**: the CLI is on PATH and runs; it is logged in (`codex login`
  or `OPENAI_API_KEY`); `codex exec --help` offers `--json` (older CLIs
  can't run headless); the user's codex config doesn't define an
  `mcp_servers.saddle` that saddle's per-turn `-c` overrides can't replace
  (a `url` transport, or `enabled = false`); and the sandbox isn't
  read-only.
- **grok**: the existing `grok` check runs whenever grok is the
  orchestrator or the harness.

When neither the orchestrator nor the harness is Claude Code, a missing
`claude` is reported OK ("not used"), and the `orchestrator allowlist` and
`orchestrator skills` checks are skipped: they configure and probe a Claude
session nobody will start.

### 5. TUI numbers

The context gauge and compact nudge read the orchestrator's transcript and
its model's window. For codex the transcript is now found by the session id
the bridge reports (codex names rollouts `rollout-<time>-<thread id>.jsonl`),
falling back to the newest rollout started in the repo root. Before this,
an owner's own codex session in the repo root could be read as the
orchestrator's. The model's context window and price come from
`usage.CodexModel` and #637's pricing, which the orchestrator gauge and the
usage pane already use for codex workers.

## Consequences

- With only Codex installed, `saddle init` writes `harness = "codex"` and
  `[orchestrator] adapter = "codex"`, doctor is clean, and `saddle up`
  starts a codex orchestrator that calls saddle's tools over MCP.
- Existing setups don't change: with claude installed and no
  `[orchestrator]` section the orchestrator is Claude Code.
- A codex orchestrator reacts to events at turn boundaries, not mid-turn.
- `saddle up --rc` (Remote Control) stays Claude-only; doctor's
  `remote control` check is unchanged.
