# The saddle Claude Code plugin

This plugin lets your own Claude Code session orchestrate saddle. It ships a
skill and commands, an MCP server (`saddle plugin mcp`), settings hooks
(`saddle plugin hook`, `bin/saddle-check`), and a **mod**: a hooks module
that runs inside Claude Code 2.1.287 or newer (epic #165).

## The mod

`hooks/hooks.json` names the hooks module under `modules` and keeps the
settings hooks under `hooks`. A session on an older Claude Code ignores
`modules`, and the settings hooks work either way.

- `hooks/register.tsx`: at session start, when the session draws a UI, the
  repo has `.saddle/` and this isn't one of saddle's worker sessions, it
  starts a timer. Every 3 s it runs `saddle queue --json`. While the engine
  lock is held (`saddle plugin engine` or `saddle up`), it also runs `saddle
  status --json`, and `saddle stack --json` every 30 s, since that one calls
  GitHub. The result is written to `$.state` as `saddle.snapshot`, and only
  when something a drawing shows has changed. That write redraws every hook
  that read the snapshot.
  It also registers `/saddle-pane` and `/saddle`, draws the saddle pane,
  draws the needs-you band and wakes the orchestrator (all below).
- `hooks/snapshot.ts`: the pure parts: parsing each command's JSON, change
  detection and the version check.
- `hooks/pane.ts`: the pane's rows for each tab, built from the snapshot.
- `hooks/needsyou.ts`: the needs-you band's items, built from the snapshot,
  and the saddle command each of its buttons runs.
- `hooks/commands.ts`: parsing `/saddle`'s arguments into the saddle command
  it runs, and its reply.
- `hooks/wake.ts`: which of the orchestrator's events wake Claude, the
  prompt that wakes it, and the routine backlog.
- `hooks/claimguard.tsx`: the claim guard for worker sessions (below): claim
  matching, the Bash write targets, its `tool.call` hook and its band.
- `types/index.d.ts`: the state contract (`SaddleSnapshot`, the pane's
  view `SaddlePane`: its tab and scroll offset, and the band's `SaddleBand`:
  the screen Peek fetched, and the claim guard's `SaddleGuard`). Later hooks
  (the pane, the needs-you band, the commands) read it with
  `read($, atom({ plugin: 'saddle', key: 'snapshot' } as const, null))`.

### The saddle pane

`/saddle-pane` shows or hides a pane with what `saddle up` shows. In the
fullscreen layout it docks beside the transcript. Without that layout it
opens inline, focused, and Escape closes it, as `/diff` does. It draws only
from the snapshot and runs nothing itself, so it updates on each write the
refresh makes.

- **Agents** (`1`): id, state, model and title, with the last activity under
  each. Finished tasks go last.
- **Claims** (`2`): each live task and its globs.
- **Train** (`3`): the merge queue, next first, with the auto-merge holds.
- **Stacks** (`4`): the auto-merge watcher, then each PR stack bottom first:
  base, CI, mergeability, merge state and how far it is behind base.

The header and the tab buttons stay put. The rows under them scroll by the
wheel (three rows a tick) or the scroll keys while the pane has the keyboard
(ctrl+x tab, or a click). Once the pane has the keyboard, the digit keys
switch tabs. Any refresh that failed shows as a `!` line at the top.

`saddle status --json` doesn't report an agent's live activity yet. Until it
does, the activity line falls back to the failure reason, the train state or
the count of pending notices.

### The needs-you band

Above the prompt, a band lists every task that waits on a person or the
orchestrator, one line each: `‼ t54 (title): what it needs`. It hides when
nothing is pending, and while a survey holds the band. Like the pane, it draws
only from the snapshot. A task shows when:

- it waits on a prompt (`needs_you`);
- it stopped without calling done (`idle`);
- it conflicted, or its tests failed in the train;
- its PR's CI is red (`ci_red` in `saddle status --json`, or failing checks in
  its stack);
- the stack sentinel flagged its PR at risk;
- it is held in the merge train.

Each line has buttons, shown where they apply:

- **Peek** (a live task with a window): runs `saddle peek <task> -n 15` and
  shows its screen under the band. Press it again, or Close, to hide it.
- **Ask Claude**: submits a `[saddle]` prompt naming the task and what it
  needs, so the orchestrator handles it.
- **Land**: `saddle land`, for a task in the train.
- **Hold** / **Release**: `saddle queue hold|release <task>` for a train
  entry, else `saddle automerge hold|release <task>` for its PR stack.

Land, Hold and Release toast the command's first line of output, or what
failed. The next refresh redraws the band.

### Instant /saddle commands

`/saddle <command>` runs one saddle command and prints a short reply. It
doesn't start a Claude turn, and it runs at once even while Claude is
working, instead of waiting for the turn to end.

| Command | Runs |
| --- | --- |
| `/saddle status` | `saddle status` |
| `/saddle land` | `saddle land` |
| `/saddle hold <task> [reason]` | `saddle queue hold <task> <reason>` for a queued task, else `saddle automerge hold <task>` for its PR stack |
| `/saddle release <task>` | `saddle queue release <task>`, or `saddle automerge release <task>` as for hold |
| `/saddle restack` | `saddle restack` |
| `/saddle automerge on\|off\|status` | `saddle automerge on\|off\|status` (bare `automerge` is `status`) |

Hold and release pick the queue or the stack from the snapshot, as the
needs-you band's buttons do. A reply longer than 30 lines is cut short.
`/saddle`, `/saddle help` or a bad command prints the usage and runs nothing.
A failed command prints its exit code and the first line of its stderr.

These are separate from the plugin's markdown commands, which Claude runs
as a turn: `/saddle:status` (Claude sets saddle up if needed and summarizes
status) and `/saddle:orchestrate`. Note the space in `/saddle status`
versus the colon in `/saddle:status`.

### The claim guard (worker sessions)

`hooks/claimguard.tsx` is an opt-in guard for saddle's worker sessions
(#171). Turn it on with the plugin's `claimGuard` option (`/config`, or
`pluginConfigs.saddle.options.claimGuard: true` in settings). It acts only
where `SADDLE_TASK` is set, so the orchestrator's own session is never
guarded.

saddle's PreToolUse hook already denies a write into another task's claims
or a serial file. The guard covers the gap: a write inside the worktree on a
path no task has claimed. It holds Edit, Write and NotebookEdit calls on such
a path and, best effort, obvious Bash writes (redirects, `tee`, `touch`,
`rm`, `cp`, `mv`, `install`, `sed -i`; it stops at a `cd` and skips words
holding `$` or a glob). The held call fails with a message asking the worker
to claim the path first. Claims match as `internal/claims` matches them: a
plain path covers everything under it, and globs match segment by segment.
It reads the claims from `saddle status --json`, and reads them again before
it holds a call, so a fresh claim lets the retry through.

It is never the only way out:

- **Claim** the path (the saddle `claim` tool), then retry.
- **Retry** the same call: the guard holds each path only once a session.
- **Proceed**: a band above the prompt lists each held call, with Proceed
  (submits a `[saddle]` prompt telling the worker it may go ahead) and
  Dismiss.
- **Fail open**: when saddle can't say what the task claimed (status fails,
  the task is unknown), the call goes through. Paths outside the worktree
  (the scratchpad, `.git`) are never held.

### Waking the orchestrator

With the mod active, the orchestrating session needs no background `saddle
plugin wait`. On each tick the mod writes a heartbeat to `.saddle/mod.alive`.
When `saddle status --json` counts pending events for the orchestrator (`t0`)
and the plugin engine runs, it takes them with `saddle plugin events --json`
and sorts them:

- **Wakes Claude**: an event that needs a decision (an `action` notice: an
  agent waiting on a prompt or stopped without calling done, a conflict,
  failed tests, red CI). The mod submits one `[saddle]` prompt with every
  such event, and the routine ones held back since the last wake under
  "Also since your last update". A prompt submitted mid-turn runs once the
  turn ends.
- **Only updates the UI**: a routine event (an `info` notice: spawned,
  finished, landed). It toasts its first line and waits, up to 20 of them,
  for the person's next message, which carries them as context, or for the
  next wake.

While the heartbeat is under 15 s old, `saddle plugin hook` (the settings
hooks) delivers nothing and `saddle plugin wait` exits at once, so no event
reaches the session twice. `saddle plugin brief` says whether the mod is
active and tells the orchestrator to run `saddle plugin wait` only when it
isn't, as on a Claude Code older than 2.1.287. Once a modded session
closes, its heartbeat goes stale and the settings hooks deliver again.
Under `saddle up` the events are the TUI orchestrator's, and the mod leaves
them alone.

## Dev loop

```sh
# Load this folder in place of the installed plugin. In an interactive
# session the folder is watched: saving a file reloads the module.
claude --plugin-dir ./plugin

# Inside that session: /plugin should list "mod active · saddle".
# Run /reload-plugins after changing hooks.json or plugin.json.

# What the module hooks and calls, and anything the engine would refuse.
claude plugin validate plugin

# The mod's tests (tests/*.test.ts), run without a live session.
claude plugin test plugin
```

`go test ./plugin` (part of `make check`) checks the manifest. When `claude`
2.1.287 or newer is on PATH, it also runs `claude plugin validate` and
`claude plugin test`.

The engine writes the API's types to `.claude-plugin/types/` (gitignored)
each time it loads the folder. After that, `tsc -p plugin` type-checks the
mod. Keep the module's calls narrow: only the `saddle` binary and
`.saddle/`. That keeps `claude plugin validate`'s list easy to review.
