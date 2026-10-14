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

URLs, prose like `and/or`, branch and ref names (`saddle/integration`), bare
words (`docs`), `.git/` and `.saddle/` paths, and lockfiles (`go.mod`,
`go.sum`, …) are ignored. A claim another one covers is dropped. If nothing
matches, the ticket has no claims and runs alone, as before. The spawn event
says `inferred claims` so you can tell the two apart.

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
- `Difficulty: hard | standard | mechanical` (or the form's **Difficulty**
  field) and `Needs: review, image-gen`: what the work needs. Saddle picks
  the provider and model from them; see below.
- `Provider: codex`: run on this adapter, overriding the chooser. Use it only
  when the ticket needs something one provider alone has, and say why.
- `Model scope: Opus`: old tickets. It is a hint, not a pin: Sonnet reads as
  standard, Haiku as mechanical, Opus as no preference (it was every ticket's
  default), and the model applies only if the chooser picks claude.

## How saddle picks the provider (#505)

Tickets say what they need; the orchestrator and autopilot share one
chooser that picks the provider. It ranks the adapters that can run here
and have quota by, in order:

1. **Needs**: adapters that declare every capability the ticket needs.
   `hooks-required` is met by adapters that run saddle's hooks (claude, grok).
2. **Difficulty**: adapters whose capability table suits it. By default
   claude suits every difficulty and the others standard and mechanical, so
   hard work prefers claude while it has quota.
3. **Kind**: spawn's `kind=`, matched against `[adapters.<name>] kinds`.
4. **Hooks** for a task whose claims share a directory with another live
   task's, so claims are enforced on every write.
5. **`[adapters] policy`**: `balanced` (the default) takes the adapter with
   the most unused five-hour budget per running worker, so idle allowances
   get used and one tick's spawns spread out. `order`, `cost` and `quota`
   are still there.

An adapter that is out of quota or can't run here is skipped. If nothing
suited has quota, the best unsuited one runs it rather than nothing.

The model is the one `[adapters.<name>] models` names for the difficulty
(claude: hard = opus, mechanical = sonnet by default), else the adapter's
default. Excluded models (`exclude_models`) are never used.

`saddle status` prints, for each live task, where it went and why, e.g.
`t12 on codex: suits mechanical, 90% of budget left, 0 running`. Autopilot's
report names the provider for every ready ticket before it spawns
(`routes`), and its spawn events say where each went.

### The capability table

```toml
[adapters]
policy = "balanced"

[adapters.codex]
budget = 2000000                        # tokens per five hours
difficulties = ["standard", "mechanical"]
capabilities = ["review"]
models = { standard = "sol", mechanical = "luna" }

[adapters.claude]
budget = 3000000
models = { hard = "opus", mechanical = "haiku" }
```

Capabilities are open words: a ticket's `Needs: image-gen` routes to an
adapter that declares `image-gen`. This is the extension point for work
beyond code. An adapter whose CLI drives another host (image generation
from another account, ChatGPT plugins via `plugin-host`, web research)
declares the capability, and tickets that need it go there. Saddle knows
`review`, `long-context`, `image-gen`, `web-research`, `plugin-host` and
`hooks-required`.
