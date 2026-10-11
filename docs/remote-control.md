# Remote control (#251)

Status: design plus a read-only spike (`internal/remote`, `saddle remote`),
with server and transport hardening (#308), auth, scopes and audit
(#309), the snapshot and needs-you queue (#310) and push (#311) done. "What exists now" below lists what is built. The remaining
follow-up tickets at the end build the rest.

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
| Other users or processes on the LAN or internet | connect to the listener | ✓ binds loopback by default; `Config.Check` refuses anything else unless **both** `allow_public = true` and `tls_cert`/`tls_key` are set (TLS 1.2+, key must be 0600). Remote machines normally come in over ssh `-L`, the ssh `stdio` forced command, or a tailnet proxy. |
| A web page in the owner's browser | `fetch` to `127.0.0.1:7431`, DNS rebinding | ✓ requests with an `Origin` header are refused (403). ✓ the `Host` must be loopback, the listen IP, or a name in `[remote] hosts` (403 otherwise). ✓ no cookies; auth is a bearer header, which a browser won't attach by itself. |
| Another local user on a shared box | read the token file, connect to loopback | ✓ tokens and the audit log live in `~/.config/saddle/remote/` (0700 dir, 0600 files). A tokens file others can read is refused, like ssh with a loose key. ✓ only SHA-256 hashes are stored, never secrets. |
| A malicious or compromised repo (a clone with a checked-in `.saddle/config.toml`) | turn the listener on | ✓ `[remote]` is read **only** from the user's config (`~/.config/saddle/config.toml`), never from the repo. ✓ `serve` refuses a repo that `saddle trust` (#215) hasn't trusted. |
| A leaked token | replay | ✓ tokens expire (default 24h, at most 30d). ✓ `saddle remote token revoke` takes effect on the next request, because the file is re-read every time. ✓ the `saddle_rc_` prefix makes leaks greppable (secret scanners). ✓ scopes limit the blast radius. ✓ a token can be limited to named repos (`--repo`). |
| Token guessing | brute force | ✓ 256-bit random secrets. ✓ constant-time compare, run over every token. ✓ failed logins are rate limited per client address (10/min by default, then 429). |
| A runaway or prompt-injected remote Claude | many calls, destructive calls | ✓ per-token rate limit (120/min). ✓ scope allowlist: unknown tools are denied. ✓ admin tools need `admin` *and* a confirmation round trip (`confirm` with a one-time code bound to the token, 2 min). ○ panic pause. |
| Repudiation ("who held my stack?") | none | ✓ every failed login and every tool call, allowed or denied, goes to the audit log (JSONL) with the token name and client address, and is mirrored into the repo's event log (`kind = remote`). ✓ a call is refused if its audit line can't be written. ✓ the log rotates (10 MiB, 5 kept by default) and `saddle remote audit` reads it. |
| Prompt injection *through* saddle output | a task title or peeked screen carrying instructions to the remote Claude | ✓ item text is clipped (500 runes), prompt options to 200, and scrubbed. ✓ peek output has escapes stripped, known secret patterns redacted, lines capped (100) and clipped, and sits between two fence lines carrying a random nonce, so the screen can't fake the closing one. ✓ the server's instructions and tool descriptions say this text is untrusted data. This risk can't be removed, only reduced, which is one more reason the default token is read-only. |

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
  transport: `saddle remote stdio --scope read --repo /src/saddle --name
  phone` (built in #308). The forced command starts in `$HOME`, hence
  `--repo`. Calls are audited as `ssh:NAME` from the `SSH_CONNECTION`
  client address.
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
| `needs_you` | read | remote-only; prompts with their text, question and options, plus t0's action notices, each with an id |
| `wait_needs_you` | read | remote-only; long-polls `needs_you` with a cursor |
| `brief` | read | |
| `peek` | read | scrubbed, capped at 100 lines, fenced as untrusted data |
| `ticket` | read | reads GitHub through the host's gh |
| `message` | act | |
| `send_keys` | act | safe-send rules: never into a pane the owner is typing in |
| `queue_move`, `queue_hold`, `queue_release` | act | |
| `automerge` | act | `status` action is read-equivalent, but the tool is act |
| `concurrency` | act | |
| `unstack`, `requeue`, `publish` | act | |
| `land`, `prs`, `restack` | land | |
| `sentinel_ack` | land | it lowers a safety hold, so it isn't act |
| `spawn`, `kill`, `autopilot` | admin | every admin tool needs a confirmation round trip |
| `confirm` | admin | remote-only; runs a held admin call (see below) |
| `claim`, `release`, `ask_owner`, `done` | local | a worker's own tools; never remote |
| `pause` (planned) | act | the panic button; see failure modes |
| `down`, config writes (planned) | admin | confirmation round trip |

### Confirmation round trip for admin tools

Every `admin` tool except `confirm` is held. The first call runs nothing.
It is audited as `pending` and returns `{confirm_required, code, tool,
arguments, expires}` plus a sentence telling the remote Claude to show the
owner what will run. `confirm {code}` then runs the held call exactly as it
was first sent. The code:

- works once;
- expires after 2 minutes (`ConfirmTTL`);
- is bound to the token's hash, so another token, even an admin one, or a
  re-issued token with the same name, can't use it;
- is checked against the token's scopes again at confirm time.

The executed call is audited as `allowed` with `confirmed, code …`; a bad,
reused, expired or foreign code is audited as a denied `confirm`. The state
lives in the handler, not the MCP session, because the HTTP transport is
stateless. Its value is the second, separate tool call: Claude Code asks the
human for permission per tool, so the owner sees `confirm` with the
description of what it will run. It doesn't stop a remote Claude that has
blanket permission. That is what scopes, short TTLs and the audit log are
for. No admin tool is served remotely yet, so today the round trip is only
exercised by tests with a fake `kill`.

### Repo allowlist

`saddle remote token create ci --repo saddle --repo /src/quark` limits a
token to those repos. An entry is a repo's directory name or its absolute
path. Relative paths are refused. A token without `--repo` reaches any repo
the host serves. A token used against a repo it doesn't list gets 403, and
the refusal is audited. `token list` shows a REPOS column.

Enforcement happens twice. A caller is only *shown* the tools its scopes
reach. A receiving middleware then checks and audits *every* `tools/call`
against the table, so a hidden tool can't be called by name.

## What the remote instance sees

- **`status`**: counts (running, needs-you, queued, landed), live tasks with
  needs-you first, train state, PR, stack at risk, CI-red holds, auto-merge
  state, orchestrator context use, warnings. Landed and killed tasks show up
  only as a count. `limits` is plan-limit usage per window (5h and weekly:
  percent of cap, state, tokens, $, time to reset) and whether launches are
  paused. `stacks` is a short graph, one line each:

  ```
  stack: main ← t1 #11 (ci red) ← t3 #13 (at risk)
  queue: t4, t5 (held)
  stack api: t6 ← t7
  ```

  The PR stack bottom up from the base, the merge train queue in landing
  order, then named stacks.
- **`needs_you`**: one item per agent waiting at a prompt, with the latest
  `Notification` hook text, plus t0's undelivered **action** notices (the
  interrupt class from #222). Reading consumes nothing. Each item has an
  `id`. A prompt item also carries `prompt` (`ask` or `trust`, from
  `DetectPrompt`), its `question` and numbered `options`, parsed from the
  agent's screen: the last run numbered 1, 2, 3… on it, so a list the agent
  printed earlier isn't taken for the options. A prompt's id hashes the
  prompt as shown (its dialog box, or the lines around the question), so it
  is stable while the prompt stands and changes when it does, even when
  only the command it asks about changed. A notice's id is `n-<notice id>`.
  The result has a `cursor` for `wait_needs_you`. Answering `{item,
  option}` is an `act` tool and arrives with the remote-safe action
  semantics (follow-up 6); the id is what binds an answer to the prompt the
  owner saw.
- **`peek`** `{task, lines}`: the last lines of an agent's terminal (default
  40, at most 100, each clipped to 400 runes). Terminal escapes and control
  characters are stripped, known secret shapes are replaced with
  `[REDACTED]` (`saddle_rc_` tokens, GitHub, GitLab, Anthropic, OpenAI,
  AWS, Slack and Google keys, JWTs, private key blocks, bearer and basic
  credentials, URL passwords, and the value of any `*TOKEN*`, `*SECRET*`,
  `*PASSWORD*`, `*API_KEY*`… assignment). The output opens with
  `<<<UNTRUSTED TERMINAL OUTPUT <nonce> …>>>` and closes with
  `<<<END UNTRUSTED TERMINAL OUTPUT <nonce>>>`; the nonce is random per
  call and returned as `fence`. The result reports `redacted` and
  `truncated`. Scrubbing is a seatbelt: a secret in a shape it doesn't
  know gets through, which is why peek needs a token and is audited.

## Push

MCP server notifications reach a connected client, but Claude Code doesn't
turn them into a prompt for the user. So push has two parts:

1. **For a remote Claude session:** `wait_needs_you {cursor,
   timeout_seconds}` long-polls, blocking until an item the cursor hasn't
   seen appears or the timeout passes (default 120s, at most 600s). It
   returns `new` (just the unseen items), `items` (everything waiting), a
   fresh `cursor`, and `timed_out`. Without a cursor it waits for items
   newer than the call. The cursor is the set of item ids, so an item that
   leaves the queue and comes back counts as new. The server re-reads the
   queue every 2s and stops when the client goes away. The session runs it
   in the background the way the plugin runs `saddle plugin wait`. No
   polling loop burns tokens. Each call is one audited tool call.
2. **For the owner's phone:** an optional `[remote.push]` webhook (ntfy topic
   or a generic URL) for **interrupt-class notices only**. Digest-class and
   silent notices stay in the digest and the log (#222). The pusher follows
   the event log, where the notice policy records each orchestrator notice
   with its class (`notice_interrupt`), from the moment it starts, so
   history is never replayed. The payload carries the repo, the task id
   (the first `tN` in the notice, else `t0`) and a one-line summary (the
   notice's first line, scrubbed, at most 200 runes), never peek output:

   ```toml
   # ~/.config/saddle/config.toml (the user config only, never a repo's)
   [remote.push]
   ntfy = "https://ntfy.sh/your-long-random-topic"  # body = summary, Title = "saddle REPO: TASK"
   # or: url = "https://example.com/hook"           # POST JSON {repo, task, class, summary, ts}
   # token = "…"                                    # optional, sent as Authorization: Bearer
   ```

   Push works with the listener off; it doesn't need `[remote] enabled`.
   The target must be https (plain http only to loopback) and redirects
   aren't followed. A failed send is logged to the event log (`kind =
   remote`) once per reason and not retried: the notice still waits in the
   orchestrator's queue and in `needs_you`. `RunManaged` runs the pusher, so
   it starts wherever the listener would (see Lifecycle). A flock per repo
   (`push-<hash>.lock` in the remote dir) keeps saddle up and the plugin
   engine from both sending.

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

## Audit log

`~/.config/saddle/remote/audit.log`, JSON lines, 0600. Past `audit_max_mb`
(default 10) it rotates: `audit.log.N-1` moves to `audit.log.N`, and the
current log to `audit.log.1`. `audit_keep` old files are kept (default 5).
Several processes may write it at once (a managed serve and ssh `stdio`
sessions). Before each write, a writer checks that `audit.log` is still the
file it has open and reopens it if another process rotated it.

`saddle remote audit` prints it oldest first, across the rotated files:
`--token NAME`, `--tool NAME`, `--denied`, `--since 1h`, `-n 50` (the
default, 0 for all), `--json`.

## Lifecycle

`remote.Service` owns the listener. It checks the bind (`Config.Check`),
takes a lock, listens (TLS when configured), serves until its context ends,
then shuts down gracefully (5s) and frees the lock.

- **Lock.** `~/.config/saddle/remote/serve-<listen>.lock` (flock) holds
  who is serving that address. Only one process serves an address.
- **Foreground `saddle remote serve`** fails at once when the lock is
  held, and the error names the holder.
- **Managed (`remote.RunManaged`)** is for `saddle up` and the plugin
  engine to run beside their other watchers. It returns at once when
  `[remote] enabled` is off, and opens nothing. When on, it waits rather
  than fails while a foreground serve holds the address or the port is
  busy, and retries every 30s. It reports each new reason once to the repo's
  event log (`kind = remote`). So a foreground serve can come and go, and
  `saddle up` takes over when it stops. Problems never stop `saddle up`.
- **Wiring:** `internal/cli/root.go`'s `startWatchers` needs one line,
  `wg.Go(func() { remote.RunManaged(ctx, a, nil) })`. #308, #310 and #311
  couldn't touch that file (another task held it), so it is the one
  remaining step for "serve and push under the engine and saddle up".
  `RunManaged` runs both the listener and `[remote.push]`, so that one line
  wires both.

## The spike, hardened

What exists now:

- `internal/remote`: the token store with repo allowlists (`token.go`),
  scope table (`scope.go`), rotating audit log (`audit.go`), `[remote]`
  config with the loopback, TLS and `allow_public` check (`config.go`),
  snapshot, limits, stacks graph and needs-you reader (`source.go`), prompt
  parsing and item ids (`prompt.go`), secret scrubbing and the peek fence
  (`scrub.go`), the `wait_needs_you` long poll (`wait.go`), the push
  webhook (`push.go`), the HTTP and MCP handler (`server.go`), the admin
  confirmation round trip (`confirm.go`), and the lifecycle, lock, managed
  runner and stdio transport (`service.go`).
- `saddle remote serve`: off unless `[remote] enabled = true` is in the user
  config. It refuses untrusted repos, and any non-loopback address unless
  `allow_public` and TLS are both set. It takes the serve lock and prints
  the `claude mcp add` line.
- `saddle remote stdio`: the ssh forced-command transport.
- `saddle remote token create [--repo …]|list|revoke`.
- `saddle remote audit`.
- Tools served: `status`, `needs_you`, `wait_needs_you` and `peek`, all
  read. Every other tool is denied, even to a token scoped for it, until
  the remote-safe action semantics above exist.

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

# or, with no listener: ~/.ssh/authorized_keys on the host
command="saddle remote stdio --scope read --repo /src/saddle --name laptop",restrict ssh-ed25519 AAAA…
# and on the other machine
claude mcp add saddle-box -- ssh box saddle remote stdio
```

A public bind, only when you mean it:

```toml
[remote]
enabled = true
listen = "100.64.0.5:7431"     # a tailnet or LAN address
allow_public = true             # required for anything but loopback
tls_cert = "/home/me/.config/saddle/remote/cert.pem"
tls_key  = "/home/me/.config/saddle/remote/key.pem"   # must be 0600
hosts = ["box.tailnet.ts.net"]  # Host names to accept besides loopback and the listen IP
audit_max_mb = 10
audit_keep = 5
```

Tests: `internal/remote/*_test.go`. The acceptance test is
`TestReadTokenCannotLand`. #308 is covered by
`TestCheckPublicNeedsAllowPublicAndTLS`, `TestCheckRefusesLooseTLSKey`,
`TestListenServesTLS`, `TestPublicHostsAreAllowlisted`,
`TestStdioServesScopedTools`, `TestServeLockIsExclusive`,
`TestServiceLifecycle` and `TestRunManagedIsOffByDefault`. #309 is covered
by `TestTokenRepoAllowlist`, `TestTokenForOtherRepoIsRefused`,
`TestAdminToolNeedsConfirmation`, `TestConfirmationIsBoundToToken`,
`TestConfirmationExpires`, `TestConfirmNeedsAdmin`, `TestAuditRotates`,
`TestAuditFollowsRotationByAnotherProcess` and `TestAuditCommand`. #310
is covered by `TestParsePromptOptions`, `TestParsePromptTakesTheLastList`,
`TestParsePromptTrust`, `TestParsePromptNone`,
`TestParsePromptScrubsAndClips`, `TestNeedsYouItemIDs`,
`TestNeedsYouHasCursor`, `TestStackGraph`, `TestLimitsLine`,
`TestScrubRedactsSecrets`, `TestScrubAssignmentsKeepTheirNames`,
`TestScrubPrivateKeyBlock`, `TestScrubLeavesOrdinaryText`,
`TestPeekOutputIsCappedScrubbedAndFenced` and
`TestPeekIsScrubbedAndFenced`. #311 is covered by
`TestWaitNeedsYouWakesOnNewItem`, `TestWaitNeedsYouTimesOut`,
`TestWaitTimeoutIsBounded`, `TestWaitNeedsYouStopsWithTheClient`,
`TestPushSendsInterruptNoticesOnly`, `TestPushNtfy`,
`TestPushFailureDoesNotLoseLaterNotices`, `TestPushTaskID`,
`TestPushConfig`, `TestRunPushIsOffByDefault` and `TestPushLockIsExclusive`.

## Follow-up tickets

1. ~~**Server and transport hardening.**~~ Done in #308, except the
   one-line `startWatchers` call (see Lifecycle).
2. ~~**Auth, scopes and audit, finished.**~~ Done in #309.
3. ~~**Snapshot and needs-you queue.**~~ Done in #310. The `answer
   {item, option}` tool belongs to 6.
4. ~~**Push.**~~ Done in #311, except the same `startWatchers` line.
5. **Multi-repo addressing.** Serve `/mcp/<repo>` from one listener, with a
   trusted-repo list. **Model scope: Opus.** Routing and trust interplay.
6. **Remote-safe action semantics.** Add explicit targets, `if_version`
   optimistic checks and idempotent verbs for every `act` and `land` tool,
   then serve them remotely. That includes `answer {item, option}`, which
   presses the option only while the prompt's id still matches. **Model scope: Opus.** This is the #209 race
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
