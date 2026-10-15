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
