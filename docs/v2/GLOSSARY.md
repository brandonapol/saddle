# Saddle v2 glossary

One meaning per word. This is the v2 vocabulary ([ARCHITECTURE.md](ARCHITECTURE.md),
[ADR 0013](../adr/0013-saddle-v2.md)). v1's words are in
[../GLOSSARY.md](../GLOSSARY.md) until the cutover (#986).

## Things

- **ticket**: a unit of approved work, usually a GitHub issue carrying the
  approval label, applied by someone with write access. Not "issue" (any
  issue) and not "task".
- **task**: saddle's record of doing one ticket, or one prompt the owner
  typed. It has one worktree, one branch and at most one PR for life, and
  one stored state. Identified by its id (`t14`), never by a commit.
- **turn**: one non-interactive run of an agent CLI with one prompt. It
  ends when the process exits.
- **attempt**: saddle's record of one turn: the command, the process, the
  log file, the outcome and the tokens used.
- **agent**: a model session doing a task, on one provider. Either a worker
  or the orchestrator.
- **worker**: an agent that changes code in a task's worktree. It ends its
  work by calling `finish` or `ask`.
- **orchestrator**: the one agent the owner talks to. It plans, spawns
  tasks and answers workers. It edits no files and is disposable: any
  session can take over from the digest.
- **digest**: the short summary of current state that starts every
  orchestrator turn.
- **driver**: the code that knows how to start, resume and read one agent
  CLI. Not "adapter" or "harness".
- **provider**: whose CLI and quota a turn runs on: `claude`, `codex` or
  `grok`. A provider is *available* or *out* (until a reset time).
- **engine**: the one process per repo that observes, plans and applies.
  Nothing talks to it; it reads the database. Not a daemon in ADR 0005's
  sense: no command depends on reaching it.
- **owner**: the human running saddle.
- **forge**: the git host. GitHub at 2.0.
- **base**: the branch every task is cut from and every PR targets (main).

## Talking to agents

- **inbox**: the queue of messages for an agent. The only way anything
  reaches one. Delivered at the start of its next turn.
- **message**: one inbox row, from the owner, the orchestrator or the
  engine.
- **interrupt**: stop the running turn so a message is delivered now.
- **question**: something an agent asked with `ask` or `ask_owner`. It
  goes to the orchestrator first, then to the owner. It has an **answer**.
- **attach**: open the real CLI on a task's session, for a human. No turn
  starts for that task meanwhile.
- **wake**: start an orchestrator turn because something needs it.

## Scheduling and landing

- **`Touches`**: the path globs a ticket says it will change. Used only by
  the scheduler. Nothing enforces it. (`Claims` is read as an alias.)
- **overlap**: two tasks' `Touches`, or two PRs' changed files, share a
  path. Overlapping tasks don't run together; overlapping PRs merge oldest
  first.
- **`after`**: a task that waits until another has *merged*. Never a
  stacked branch.
- **slot**: room for one running worker.
- **route**: choose a provider and model for a task from its difficulty.
- **check**: the repo's own gate command (`make check`), run before a
  branch is pushed. CI's results are **checks** on the PR.
- **publish**: check, rebase onto base, validate against the repo's rules,
  push, open or update the PR.
- **repo rules**: what the repo wrote down about PRs, commits and merging:
  the PR template, conventions, attribution, merge method, required checks
  and reviews.
- **fix in place**: red CI or a conflict goes back to the same task, on the
  same branch and PR.
- **merged**: the PR is in base. Only this counts as done. The forge does
  it; saddle asks.
- **held**: the owner or orchestrator said not to start a task, or not to
  merge its PR, until **released**.
- **retry**: a failed action waiting to run again with backoff. Also the
  command that sends a blocked or closed task back to work.

## Modes

- **automerge**: saddle asks the forge to merge each PR when its rules are
  met.
- **autopilot**: the engine turns approved tickets into tasks while slots
  are free.
- **infinite mode**: when autopilot runs dry, the orchestrator is woken to
  find and file more work.
- **loop**: a prompt put in an agent's inbox on a schedule, until a
  condition.
- **budget**: a limit past which no new turn starts.
- **pause**: start no turns; running ones finish.

## Task states

A task is in exactly one state, with a reason.

| State | Meaning | Reasons |
|---|---|---|
| `ready` | waiting to start | `slot`, `after:<task>`, `overlap:<task>`, `no_provider`, `held` |
| `working` | a turn is running or about to | `new`, `message`, `conflict`, `ci`, `check`, `rules` |
| `blocked` | waiting on an answer | `question`, `gave_up`, `turn_failed` |
| `pr_open` | published, not merged | `checks`, `review`, `queue`, `held` |
| `merged` | on base; terminal | |
| `closed` | ended without merging; terminal | `killed`, `no_change`, `pr_closed` |

Once #959 lands, this table is generated from the transition table
(`saddle states`).

## Words v2 does not use

landed, layer, stack, restack, repair, train, integration branch, claim,
sentinel, parked, idle, needs_you, decision card, narrator, harness,
adapter, window. If you need one, the design has probably drifted: check
ADR 0013 first.
