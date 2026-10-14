# Tickets and claims

Autopilot (`saddle autopilot on`) spawns a worker for each ready ticket (an
open issue labelled `saddle:ready`). Each worker runs with path **claims**:
globs it reserves so no other worker edits them. Saddle runs a task that has
no claims alone, because it might touch anything. So claims are what let a
four-slot lane run four workers at once (#504).

## Declaring claims

Put a `Claims:` line in the ticket body with the globs the work changes:

```
Claims: internal/autopilot/**, docs/tickets.md
```

Any of these forms work:

- `Claims: a/**, b.go` or `**Claims:** a/** b.go` on one line;
- `Claims:` followed by a bullet list, one glob per bullet;
- the `### Claims` section the GitHub issue forms render (the **Claims**
  field in the Feature, Bug, Chore and Performance templates). One glob per
  line; `_No response_` counts as empty.

A declared line always wins over inference. To make a ticket run alone on
purpose, declare `Claims: **`.

## Inferred claims

With no `Claims:` line, autopilot reads the paths the body names and checks
them against the repo:

| The body names | Claim |
| --- | --- |
| a directory (`internal/claims`, `docs/`) | `dir/**` |
| a Go file (`internal/app/app.go:52`) | its package, `internal/app/**` |
| any other file (`docs/runq.md`) | the file |
| a path that doesn't exist yet under an existing top directory | the file, or `dir/**` without an extension |
| a Go import path (`github.com/org/repo/internal/x`) | the longest suffix the repo has |
| a glob (`internal/autopilot/**`) | the glob |

URLs, prose like `and/or`, branch and ref names (`saddle/integration`),
bare words (`docs`) and lockfiles (`go.mod`, `go.sum`, …) are ignored. A
claim another one covers is dropped. If nothing matches, the ticket has no
claims and runs alone, as before. The spawn event says `inferred claims` so
you can tell the two apart.

Claims only reserve. A worker may still write a file nobody claimed; the
claim hook claims it for the worker then. A narrow guess costs a later
collision check, not a blocked worker.

## How autopilot picks tickets

Every tick it walks the ready queue oldest first and spawns each ticket
whose claims overlap no running or queued task, nor any ticket it already
spawned this tick, until the lane's cap is full. Overlapping tickets stay
queued and start once the task they overlap is done. A ticket with no
claims waits until nothing else runs, and nothing else starts beside it.

Other ticket lines autopilot reads:

- `After: #12, #7`: wait until those issues' tasks are done, and build on them.
- `Model scope: Sonnet`: the model the worker gets (Opus otherwise).
