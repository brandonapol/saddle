# Config: the CPU and RAM gauge

`saddle up` draws a two-line gauge directly above the orchestrator box so you
can see whether it is safe to open another terminal that uses an AI. The
machine is often overloaded (load well above the core count), which makes
gates time out and agents crawl. The gauge is the same load reading the
heavy-run gate already takes, plus memory, on screen.

It samples every few seconds on a background tick. Drawing the screen never
reads `/proc`. A narrow terminal collapses the gauge to one line. With
`NO_COLOR` the words still say what to do. `[tui.resources] enabled = false`
hides it. If the sample can't be read, the gauge hides rather than showing a
stale or empty reading.

```toml
[tui.resources]
# enabled = true
# ok_load = 0.7       # load per core under this, with enough RAM: "OK to open another"
# busy_load = 1.2     # load per core above this: "Overloaded: don't"; between is "Busy: wait"
# ok_ram_pct = 20     # available RAM above this percent of MemTotal is fine
# busy_ram_pct = 10   # available RAM under this percent is overloaded; between is busy
```

A zero cutoff means the default, so a file that only sets `enabled = false`
keeps the other defaults. The worse of CPU and RAM wins.

CPU is the 1-minute load average divided by the core count, as a bar and a
number (`load 6.1 / 8 cores`). RAM is used against total, with free taken from
`/proc/meminfo` `MemAvailable` (`9.8 / 31 GB free 21`). Swap is shown only when
some is in use. On macOS, where `/proc` is missing, the same numbers come from
`sysctl vm.loadavg`, `vm_stat` and `vm.swapusage`.

Next to the verdict is how many saddle agents are running (each one is a heavy
process tree) and, when the heavy-run queue has holders, which classes hold a
slot.

## The heavy-run queue's admission: `[run_queue]`

The machine-wide heavy-run queue (`saddle run`, docs/resource-queue.md) holds
a run while the box is past the same readings the gauge shows. Its limits sit
in `[run_queue]`; the repo's `.saddle/runq.toml` and then
`~/.config/saddle/runq.toml` override them key by key.

```toml
[run_queue]
# max_load_per_cpu = 1.0   # hold while load per core is over this
# max_cpu_pressure = 60    # ... or CPU PSI some avg10 is over this %
# min_free_ram_pct = 10    # ... or available RAM is under this % (the gauge's busy_ram_pct)
# budget = 4               # units all heavy runs share (default half the cores, at least 3)
# [run_queue.weights]
# flutter-test = 3         # units one run of a class uses; unnamed classes weigh 1
```

`0` turns a check, or the budget, off. A run starts only when its class has
a free slot, its weight fits in the budget, and every check passes. The load
checks give up after `gate_max_wait` (10m), and `saddle run --max-wait` or
`--no-queue` bound the rest.

## The git provider: `[forge]`

Saddle drives its git host through a CLI: `gh` for GitHub, `glab` for GitLab
(docs/gitlab.md). With `[forge]` unset it goes by the origin remote's host: a
host naming GitLab selects GitLab, any other GitHub. `saddle init` and
`saddle doctor` say what they found and the command that fixes a missing or
logged-out CLI.

```toml
[forge]
# provider = "github"        # github | gitlab
# cli = "gh"                 # gh | glab | none (none: local-only, no PRs)
# host = "gitlab.example.com"  # a self-hosted GitLab: glab --hostname
```

Set `provider = "gitlab"` for a self-hosted GitLab whose host name doesn't
say "gitlab", and `host` when it differs from the remote's host (glab also
reads `GITLAB_HOST`). Each provider takes its own CLI, so `provider =
"github"` with `cli = "glab"` (or the reverse) is a config error.

## The orchestrator's CLI: `[orchestrator]`

`saddle up` runs the orchestrator on Claude Code when it is installed. Set
`adapter` to choose another CLI with a headless session (#668); `model` is the
orchestrator's model on that adapter.

```toml
[orchestrator]
# adapter = "codex"        # claude | codex | grok | opencode; empty: claude if installed,
#                          # else the first headless agent that can run
# model = "gpt-5-codex"    # needs adapter; empty is the adapter's own key
#                          # ([claude]/[grok] orchestrator_model, [adapters.codex]/[adapters.opencode] model)
# compact_at = 0.7         # context fraction at which it is told to compact
# remote_control = false   # saddle up --rc (docs/remote-control.md)
```

Any other adapter name is a config error. A quota failover still wins for the
run it happens in. `saddle doctor` has an `orchestrator` row that names the
CLI and checks what it needs; for codex that includes `exec --json`, a login
and a sandbox `gh` and `saddle` can work in. Design:
[ADR 0002](adr/0002-orchestrator-on-any-agent.md).

## Environment variables

| Variable | Effect |
|---|---|
| `SADDLE_ROOT`, `SADDLE_TASK` | Set by saddle on every agent process: the repo and the task id. `SADDLE_ROOT` wins when saddle looks for the repo. Saddle finds and kills leaked processes by these tags. A `saddle run` without `SADDLE_TASK` shows as foreign. |
| `SADDLE_RUNQ=off\|observe\|enforce` | Overrides the heavy-run queue's mode for one shell. |
| `SADDLE_SYMBOL_BUDGET=<duration>` | Replaces the time budget of every symbol-claim rule (250ms for rules 1 and 2, 300ms for rule 3) with a positive Go duration such as `2s`. Past its budget a rule keeps the path verdict, so a loaded machine can deny a share the rules would allow; the e2e worlds raise it. Invalid or non-positive values are ignored. |
| `SADDLE_E2E_DIR` | Set by the e2e harness on its test processes (tmux servers, fake agents) to the test's temp dir. `saddle doctor` and `saddle up` use it to find leaks. Not for users. |
| `SADDLE_HOOK_DEPTH` | Internal: saddle's hook scripts count how deeply they nest. See `docs/FAILURE-MODES.md`. |
| `SADDLE_REFGUARD=off` | Operator recovery for the ref guard, outside a task (logged). |
