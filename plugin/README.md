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
  It also registers `/saddle-pane` and draws the saddle pane (below).
- `hooks/snapshot.ts`: the pure parts: parsing each command's JSON, change
  detection and the version check.
- `hooks/pane.ts`: the pane's rows for each tab, built from the snapshot.
- `types/index.d.ts`: the state contract (`SaddleSnapshot`, and the pane's
  view `SaddlePane`: its tab and scroll offset). Later hooks
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
