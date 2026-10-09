# Remote control (#251)

Status: design plus a read-only spike (`internal/remote`, `saddle remote`).
Nothing in this doc that goes beyond the spike exists yet. The follow-up
tickets at the end build it.

## Goal

Let Claude Code sessions other than the one hosting `saddle up` look at a
running saddle and steer it. That means another terminal on the same box,
a session on another machine, the owner's phone, or a teammate. They should
be able to see agents, stacks and the needs-you queue, answer a question,
hold a stack, toggle auto-merge, change the bots limit, and land.

Default posture: **off, loopback only, read-only, every call authenticated
and audited.**

## Threat model

Assets, in order of damage if abused:

1. **The owner's GitHub and machine.** `land`, `prs`, `restack` and auto-merge
   push to and merge on GitHub. `send_keys` types into agent panes, and an
   agent runs with the owner's permissions. Code execution on the host is
   therefore one tool call away.
2. **Repo contents and secrets.** `peek` shows terminal screens, which can
   show env vars, tokens or private code. `brief` and `status` show paths and
   task text.
3. **Availability.** `kill`, `down`, a held stack, or a flood of calls can
   stop the owner's work.

Attackers considered:

| Attacker | Vector | Mitigation (spike ✓ / follow-up ○) |
|---|---|---|
| Other users or processes on the LAN or internet | connect to the listener | ✓ binds loopback only; `CheckListen` refuses anything else. Remote machines come in over ssh `-L` or a tailnet proxy, never a public bind. ○ a non-loopback bind would need TLS plus an explicit `allow_public = true`. |
| A web page in the owner's browser | `fetch` to `127.0.0.1:7431`, DNS rebinding | ✓ requests with an `Origin` header are refused (403). ✓ the `Host` must be loopback (403 otherwise). ✓ no cookies; auth is a bearer header, which a browser won't attach by itself. |
| Another local user on a shared box | read the token file, connect to loopback | ✓ tokens and the audit log live in `~/.config/saddle/remote/` (0700 dir, 0600 files). A tokens file others can read is refused, like ssh with a loose key. ✓ only SHA-256 hashes are stored, never secrets. |
| A malicious or compromised repo (a clone with a checked-in `.saddle/config.toml`) | turn the listener on | ✓ `[remote]` is read **only** from the user's config (`~/.config/saddle/config.toml`), never from the repo. ✓ `serve` refuses a repo that `saddle trust` (#215) hasn't trusted. |
| A leaked token | replay | ✓ tokens expire (default 24h, at most 30d). ✓ `saddle remote token revoke` takes effect on the next request, because the file is re-read every time. ✓ the `saddle_rc_` prefix makes leaks greppable (secret scanners). ✓ scopes limit the blast radius. |
| Token guessing | brute force | ✓ 256-bit random secrets. ✓ constant-time compare, run over every token. ✓ failed logins are rate limited per client address (10/min by default, then 429). |
| A runaway or prompt-injected remote Claude | many calls, destructive calls | ✓ per-token rate limit (120/min). ✓ scope allowlist: unknown tools are denied. ○ destructive tools need `admin` *and* a confirmation round trip. ○ panic pause. |
| Repudiation ("who held my stack?") | none | ✓ every failed login and every tool call, allowed or denied, goes to the audit log (JSONL) with the token name and client address, and is mirrored into the repo's event log (`kind = remote`). ✓ a call is refused if its audit line can't be written. |
| Prompt injection *through* saddle output | a task title or peeked screen carrying instructions to the remote Claude | ○ item text is clipped (500 runes) today. ○ peek output is scrubbed of known secret patterns and fenced as data. This risk can't be removed, only reduced, which is one more reason the default token is read-only. |

Out of scope: a compromised host account (it owns everything anyway), and
the safety of the remote Claude Code's own machine.

## Remote surfaces considered

**(a) Claude Code Remote Control on the orchestrator session.** Claude Code
can expose a running local session to claude.ai and the mobile app. Run on
the orchestrator session (the plugin's, or `saddle up`'s chat pane), it already
reaches every orchestrator MCP tool. It needs nothing from saddle, and auth is
the owner's claude.ai account. Its limits:

- It is *the* orchestrator session, so there is one controller, and it has
  full power. There are no scopes and no read-only view to hand a teammate.
- Answering from the phone types into the orchestrator's own context and
  costs its tokens.
- It is gone when that session or the tmux session is (#247).
- Needs-you items reach the phone only as fast as the orchestrator notices
  them.

Saddle should **document it as the zero-build path for the owner alone**. It
is not the general answer.

**(b) MCP over the network.** A `saddle remote serve` process speaks MCP
streamable HTTP. Any Claude Code instance adds it with
`claude mcp add --transport http … --header "Authorization: Bearer …"`. It
gets real per-token scopes, several controllers at once, and an audit trail.
Later it can stream to clients (SSE) for push. A unix socket was rejected:
Claude Code's HTTP transport takes a URL, not a socket path, so a socket
would need a forwarder anyway.

**(c) A thin remote CLI over ssh** (`ssh box saddle status --json`, or
`saddle --host user@box …`). It needs no new server and uses ssh's
authentication. A remote Claude calls it through Bash. Its limits: Bash
permission prompts for every call, no structured tool schemas, and anyone
with ssh access has a full shell anyway. Scopes are advisory unless you use
an ssh forced command.

**(d) The plugin in a remote checkout talking to the host.** This needs (b)
or (c) underneath, plus a second copy of the repo. It adds nothing.

### Decision

- **Primary: (b), MCP over streamable HTTP on a loopback address.** Remote
  machines reach it through `ssh -L 7431:127.0.0.1:7431 box` or a tailnet
  (for example `tailscale serve`, which adds TLS and tailnet identity in
  front). This is what the spike builds.
- **Fallback: stdio MCP over ssh with a forced command.** Add the server with
  `claude mcp add saddle-box -- ssh box saddle remote stdio`, and give the
  key a line in `authorized_keys` like
  `command="saddle remote stdio --scope read",restrict ssh-ed25519 …`. There
  is no listener at all, ssh authenticates, and the forced command pins the
  scope. It reuses the same scoped server (`newServer`) over a stdio
  transport, which is a small follow-up.
- **Also documented: (a)** for the owner who only wants their phone.

## Token and scope model

A token is a name, a set of scopes, an expiry and a SHA-256 of its secret. It
is stored in `~/.config/saddle/remote/tokens.json`, mode 0600, written
atomically. It is never stored in the repo, and its secret is never stored
anywhere. `saddle remote token create NAME --scope act --ttl 8h` prints the
secret once.

| Scope | Grants | Implies |
|---|---|---|
| `read` | see state | (always included) |
| `act` | steer without landing | read |
| `land` | move the stack and GitHub | read |
| `admin` | start and stop work, config | everything |
| `local` | saddle's own workers' tools; no token can hold it | n/a |

**Recommended default:** `read`, 24h TTL. The owner's own remote sessions get
`read,act` for a working day. `land` and `admin` are minted on demand with a
short TTL (for example `--ttl 1h`).

### Every tool mapped to a scope

This is the source of truth for `remote.ToolScopes`. `TestEveryMCPToolHasAScope`
fails when an MCP tool is added without a scope.

| Tool | Scope | Notes |
|---|---|---|
| `status` | read | remote returns a compact snapshot (no worktree paths or window ids) |
| `needs_you` | read | remote-only; prompts with their text, plus t0's action notices |
| `brief` | read | |
| `peek` | read | follow-up: scrub secrets, cap lines |
| `ticket` | read | reads GitHub through the host's gh |
| `message` | act | |
| `send_keys` | act | safe-send rules: never into a pane the owner is typing in |
| `queue_move`, `queue_hold`, `queue_release` | act | |
| `automerge` | act | `status` action is read-equivalent, but the tool is act |
| `concurrency` | act | |
| `unstack`, `requeue`, `publish` | act | |
| `land`, `prs`, `restack` | land | |
| `sentinel_ack` | land | it lowers a safety hold, so it isn't act |
| `spawn`, `kill`, `autopilot` | admin | `kill` needs a confirmation round trip |
| `claim`, `release`, `ask_owner`, `done` | local | a worker's own tools; never remote |
| `pause` (planned) | act | the panic button; see failure modes |
| `down`, config writes (planned) | admin | confirmation round trip |

Enforcement happens twice. A caller is only *shown* the tools its scopes
reach. A receiving middleware then checks and audits *every* `tools/call`
against the table, so a hidden tool can't be called by name.

## What the remote instance sees

- **`status`**: counts (running, needs-you, queued, landed), live tasks with
  needs-you first, train state, PR, stack at risk, CI-red holds, auto-merge
  state, orchestrator context use, warnings. Landed and killed tasks show up
  only as a count. Follow-up: usage and plan limits, and stacks as a short
  graph.
- **`needs_you`**: one item per agent waiting at a prompt, with the latest
  `Notification` hook text, plus t0's undelivered **action** notices (the
  interrupt class from #222). Reading consumes nothing. Follow-up: give each
  item an id and the answer options (parsed with `DetectPrompt`) so `act` can
  answer `{item, option}` instead of raw keys.
- **`peek`** (follow-up): on demand, truncated, secrets scrubbed, wrapped as
  untrusted data.

## Push

MCP server notifications reach a connected client, but Claude Code doesn't
turn them into a prompt for the user. So push has two parts:

1. **For a remote Claude session:** a `wait_needs_you` tool that long-polls,
   blocking until a new item appears or a timeout passes. The session runs it
   in the background the way the plugin runs `saddle plugin wait`. No polling
   loop burns tokens.
2. **For the owner's phone:** an optional `[remote.push]` webhook (ntfy topic
   or a generic URL) fired by the engine for **interrupt-class notices only**.
   Digest-class notices stay in the digest (#222). The payload carries the
   task id and a one-line summary, never peek output.

## Concurrency and consistency

Two controllers (the local TUI and a remote one) will act at once.
Last-writer-wins toggles are the #209 bug class. The rules for every action
tool served remotely:

- **Explicit targets.** No "the current stack": every call names its task,
  stack or PR.
- **Optimistic versions.** `status` returns a `version` per mutable object
  (stack hold, automerge, concurrency, queue). Actions take `if_version` and
  fail with the current state when it moved.
- **Idempotent verbs.** `hold` on a held stack is a no-op success, not a
  toggle.
- **Safe send.** `send_keys` from a remote session goes through the same guard
  as the orchestrator's (no typing into a pane with a draft). An answer is
  bound to a needs-you item id, so a prompt that has already changed is not
  answered.
- **Visibility.** The TUI header shows connected controllers (token names
  seen in the last few minutes). Remote actions appear in the event log
  with the token name.

## Multi-repo

One host runs several repos (saddle and quark). The decision is **one listener
per host that addresses repos by name**: `/mcp/<repo>`, or a `repo` argument
on every tool. Each repo must be trusted and listed in
`[remote] repos = [...]`. Tokens carry an optional repo allowlist. The spike
serves the one repo in its working directory. Running one listener per repo
on separate ports is the stopgap.

## Failure modes

- **Host asleep or offline:** the remote sees a connection error. Nothing is
  queued remotely, so there is nothing to replay. Calls are idempotent, so
  retrying is safe.
- **tmux session vanished (#247):** `status` keeps working, because it reads
  the store. `peek` and `send_keys` report the session as gone, and `needs_you`
  shows the parked tasks.
- **Server restart:** the server is stateless (one MCP session per request), so
  clients reconnect on their own.
- **Revocation:** takes effect on the next request (tested).
- **GitHub-only state:** the remote sees what the host last cached (CI, PR
  state), with its age. It can't reach GitHub through saddle for anything
  else.
- **Panic:** `saddle remote pause` (local) or the `pause` tool (`act`) stops
  accepting actions and stops auto-merge and autopilot until the owner runs
  `saddle remote resume` locally. Reads keep working. It is the self-service
  escape hatch AGENTS.md asks for: one local command undoes it.

## The spike

What exists now:

- `internal/remote`: the token store (`token.go`), scope table (`scope.go`),
  audit log (`audit.go`), `[remote]` config and loopback check (`config.go`),
  snapshot and needs-you reader (`source.go`), and the HTTP and MCP handler
  (`server.go`).
- `saddle remote serve`: off unless `[remote] enabled = true` is in the user
  config. It refuses non-loopback addresses and untrusted repos, and prints
  the `claude mcp add` line.
- `saddle remote token create|list|revoke`.
- Tools served: `status` and `needs_you`. Every other tool is denied, even
  to a token scoped for it, until the remote-safe action semantics above
  exist.

Try it:

```sh
# ~/.config/saddle/config.toml
[remote]
enabled = true
listen = "127.0.0.1:7431"

saddle remote token create phone            # prints the secret once
saddle remote serve                         # in the repo
# on another machine:
ssh -N -L 7431:127.0.0.1:7431 box &
claude mcp add --transport http saddle-box http://127.0.0.1:7431/mcp \
  --header "Authorization: Bearer saddle_rc_…"
```

Tests: `internal/remote/*_test.go`. The acceptance test is
`TestReadTokenCannotLand`.

## Follow-up tickets

1. **Server and transport hardening.** Run `serve` under the engine and
   `saddle up` with a lifecycle and lock. Add the ssh forced-command `stdio`
   mode and an optional TLS plus `allow_public` gate. **Model scope: Opus.**
   Security-sensitive lifecycle and transport work.
2. **Auth, scopes and audit, finished.** Add a per-token repo allowlist, a
   confirmation round trip for `admin` tools, audit rotation, and
   `saddle remote audit`. **Model scope: Opus.** Permission design.
3. **Snapshot and needs-you queue.** Add item ids, parsed answer options,
   usage and limits, a stacks graph, and `peek` with secret scrubbing.
   **Model scope: Opus.** Prompt parsing and scrubbing need judgment.
4. **Push.** Add the `wait_needs_you` long-poll and a `[remote.push]` ntfy or
   webhook for interrupt-class notices only. **Model scope: Opus.** Ties
   into the #222 notice policy.
5. **Multi-repo addressing.** Serve `/mcp/<repo>` from one listener, with a
   trusted-repo list. **Model scope: Opus.** Routing and trust interplay.
6. **Remote-safe action semantics.** Add explicit targets, `if_version`
   optimistic checks and idempotent verbs for every `act` and `land` tool,
   then serve them remotely. **Model scope: Opus.** This is the #209 race
   class.
7. **TUI indicator of connected controllers.** Show token names seen recently
   in the header, and remote events in the log view. **Model scope: Sonnet.**
   A small UI read of existing events.
8. **Doctor check.** Report what is exposed: enabled or not, the listen
   address, live tokens and their scopes, and warn on loose file modes or
   long TTLs. **Model scope: Sonnet.** A mechanical check over existing
   functions.
9. **Panic pause.** `saddle remote pause|resume` and the `pause` tool.
   **Model scope: Opus.** It touches automerge, autopilot and the action
   path.
10. **e2e journeys.** Two controllers racing, a revoked token, panic pause,
    and a read token denied land, all over a real listener. **Model scope:
    Opus.** Multi-process e2e.
