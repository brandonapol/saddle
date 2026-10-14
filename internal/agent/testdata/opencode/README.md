# opencode panes (#509)

Captured from opencode 1.19.0 in a 120x40 tmux pane with `capture-pane -p`
(`.txt`) and `capture-pane -p -e` (`.ansi`, with SGR escapes). Paths are
rewritten to `/repo/wt`.

- `busy.txt`: a turn in progress. The footer shows `esc interrupt`.
- `idle.txt`: the turn finished; the footer shows the cwd and context use.
- `approval.txt`: `permission: {"bash": "ask"}`; the `△ Permission required`
  panel with `Allow once   Allow always   Reject`.
- `worker-idle.txt`: a worker started by the adapter's launch.sh (`--auto`;
  the composer reads `Build auto · <model>`) after its task.
- `draft.txt`: the same pane with text typed into the composer.
- `quota.txt` is **not captured**: limits can't be hit on demand. Its three
  messages are opencode 1.19.0's own strings (free tier, Go usage limit with a
  relative reset, provider `insufficient_quota`), placed where a turn's reply
  goes. Retries show `[retrying in <n> attempt #<k>]` on the status line.
