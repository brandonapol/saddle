# Scratch: where saddle's temp files go

Saddle keeps all of its temp files under one **scratch root** and sweeps it by
itself, so it never fills `/tmp`. On many machines `/tmp` is a small tmpfs.
Once it filled, every gate failed with `disk quota exceeded` (#320, #322).

## The root

The root is `[tmp] dir`. When that is unset it is `<user cache dir>/saddle/tmp`,
which is `~/.cache/saddle/tmp` on Linux. A `[tmp] dir` inside the repo is
ignored. Saddle reads `[tmp]` through its config package like every other
section: the user's `~/.config/saddle/config.toml` first, then the repo's
`.saddle/config.toml`, key by key. A relative `dir` is relative to the repo.
The running sweeper re-reads it on each check, so an edit needs no restart.

```toml
[tmp]
# dir = "~/.cache/saddle/tmp"   # the scratch root
# max_age = "60m"               # how old an entry must be before a sweep removes it
# low_free_pct = 15             # free space under this % on the root's disk holds spawns; -1 turns it off
# max_bytes = "20G"             # cap the root as if it were its own disk; free space is what its files leave
```

`saddle up`, `saddle mcp` (the MCP server every agent and the orchestrator
run), `saddle spawn` and `saddle land` set `TMPDIR` and `GOTMPDIR` to the root
when they start. Everything they start inherits it:

- **The train** runs inside the orchestrator's MCP server or `saddle land`.
- **Gates.** The train's test and lint gates, the pre-publish gate and
  done's lint gate each run in a fresh per-run dir, `<root>/<repo hash>/gate-*`.
  That dir is removed afterwards. `[train] tmpdir` still overrides the parent
  dir for the gates.
- **Agents.** Each spawned agent gets its own dir, `<root>/<repo hash>/task-<id>`,
  and its window starts with `TMPDIR`, `GOTMPDIR` and `CLAUDE_CODE_TMPDIR`
  pointed there (#327), so `make check` in an agent's worktree compiles and
  tests under the root. Its brief tells it to keep scratch, clones and
  downloads under `$TMPDIR`, never at a hardcoded `/tmp/<name>`. Killing or
  landing the task removes the dir. `done` keeps it, since the agent may be
  sent back to fix its branch.

To run a check by hand the way saddle does, export the same variables:

```sh
export TMPDIR=~/.cache/saddle/tmp GOTMPDIR=~/.cache/saddle/tmp
make check
```

## The sweep

These remove stale scratch:

- `saddle up`, once when it starts and then every 10 minutes.
- A task's `done` and `kill`, in the background.
- `saddle gc`. With `--dry-run` it only lists what it would remove.

A sweep only looks at two kinds of entries:

1. Top-level entries of the root. A repo dir's children are swept one by one;
   the repo dir itself is never removed.
2. Entries in the OS temp dir that you own and that carry one of saddle's own
   prefixes: `go-build*`, `e2ebin*` and `saddle-*`. "OS temp dir" means the
   `TMPDIR` saddle started with, or `/tmp`.

A task's dir is the exception to both rules below. While its task is live
the sweep never removes it, however old. Once the task is killed, landed,
failed or gone, the sweep removes it however fresh, unless a live process
holds it.

It removes an entry only when both of these hold:

- Neither the entry nor any of its direct children changed within `max_age`.
- No live process holds it. On Linux saddle reads `/proc/*/cwd`, `/proc/*/exe`
  and `/proc/*/fd` to check this. Elsewhere it goes by age alone.

The sweep never touches anything else in `/tmp`. It never follows a symlink and
never removes the root itself.

## Low space

Before each spawn and on every sweep tick, saddle measures the free space on
the disk that holds the root. With `max_bytes` set, the root also counts as a
disk of that size on its own, and whichever is fuller counts. When free space
is under `low_free_pct`:

1. Saddle sweeps again with a 5-minute age threshold.
2. If the disk is still low, new spawns are held. Spawn returns
   `scratch space low`, and the orchestrator gets an action notice. The notice
   names the biggest entries, says whether each one is saddle's, and says how
   to get out. `saddle status` shows a warning line too.
3. The hold lifts by itself once there is room. Spawn's `force` overrides it.

When a gate still fails on temp space (`disk quota exceeded`, `no space left
on device`), its message names the temp entries with the most files. That way
a leak like #320's 437k empty dirs shows up straight away.

## Visibility

- `saddle doctor` has a **scratch** row. It shows the root's size and free
  space, and warns when the disk is low. It also warns when saddle's own entries
  in the OS temp dir changed recently, which means some path still writes
  there, and when an agent's `TMPDIR` is outside the root.
- `saddle status` warns while the disk is low.
- `saddle gc --dry-run` lists what the sweep would remove and what it keeps
  because a live process holds it.
