# ADR 0010: Unattended workers run headless, one invocation per attempt, with a typed result

- Status: Proposed
- Issue: #804. Related: ADR 0004 (I5, cluster C6), ADR 0009, #827, #826,
  #780, #781, #782, #786, #790, #798

## Context

A worker today is an interactive Claude Code or Codex TUI in a tmux
window, and saddle infers its state from the screen and the hooks. That
inference is behind most of ADR 0004's cluster C6: a stopped agent read as
working (#780), windowless `running` tasks (#781), permanent resume
give-ups (#782), `needs_you` with no timeout (#786), prompts saddle can't
answer (#790), and login expiry seen only as a screen (#798). Every one of
them needed a heuristic and a timeout guessed after the fact. The idea
comes from mattpocock/sandcastle (MIT, design only), where each agent run
is one non-interactive invocation that ends on a result or a timeout.

## Decision

1. **`[spawn] mode = "headless" | "tmux"`.** Autopilot and infinite mode
   default to headless; a task spawned by the owner or the orchestrator in
   an attended session defaults to tmux. tmux stays fully supported.
2. **One invocation per attempt.** A headless worker is one process in its
   worktree (`claude -p --output-format stream-json`, `codex exec --json`,
   the Grok equivalent), with the brief as the prompt. Its stream goes to
   `.saddle/run/<task>/log.jsonl`, and the TUI and `peek` show it
   read-only. There is no screen to read and no prompt to answer.
3. **A typed result ends the attempt.** The worker ends with
   `<saddle-result>{"status":"done|no_change|blocked", "summary", "question"}</saddle-result>`,
   validated against a schema. `done` runs the done gate, `no_change`
   closes the task, and `blocked` opens one decision card with the
   question. A missing or invalid result is a failed attempt.
4. **Timeouts, not heuristics.** No stream output for the idle timeout
   fails the attempt. A result followed by a process that won't exit is a
   success, and saddle kills the process group. These are the exits that
   #827's table declares for a headless `running` task.
5. **Permissions are decided before the run.** The allowlist goes on the
   command line, and a denied tool is a tool error in the stream, never a
   modal.
6. **Failed attempts are retried by #826's primitive.** Timeout, crash,
   auth or quota errors are classified from the exit code and stream,
   retried with backoff, resumed with the same session id, and moved to
   another adapter when quota is the cause (through #828's spawn policy).
7. **`saddle attach <task>`** opens an interactive session on the same
   worktree and session id for a human, between attempts.

The PR-graph engine (ADR 0009) doesn't change: it sees a change when the
task calls `done`, whichever mode ran it.

## Alternatives considered

- **Keep tmux and improve the screen heuristics.** That is the path that
  produced the tickets above, one heuristic each.
- **Headless only, as sandcastle did in v1.** Rejected: the owner works
  attended sessions in tmux, and the orchestrator answers agents there.

## Consequences

- #780, #781, #782, #786, #790 (the prompt half) and #798 become moot for
  headless tasks; their tmux halves stay with #827's table.
- An agent can no longer ask mid-run in headless mode: a question ends the
  attempt as `blocked`, and the answer resumes the session.
- Each adapter needs a headless contract test (stream format, result,
  exit codes), and a fake adapter that hangs, crashes, hits quota and emits
  prompts for #804's overnight soak.
- Open for the owner: the idle timeout default, whether infinite mode may
  ever use tmux, and whether `blocked` should wait for the orchestrator's
  answer before the owner's.
