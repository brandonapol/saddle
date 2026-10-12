# Blocker issues

Every blocker saddle escalates to the owner becomes a GitHub issue (#407).
The same failure recurring adds a comment to that issue and bumps its count,
so you get a record of what keeps hurting instead of chat lines that are gone
by the next session.

## What triggers a filing

The escalation points already write an event when they escalate. While
`saddle up` runs, the sentinel sweeps new events each cycle and files one
issue per escalation:

| Event | Kind | Raised by |
|---|---|---|
| `ci_red_escalated` | `ci_red` | CI is still red after `[ci] repair_attempts` repairs |
| `history_card` | `history_check` | a history-scanning check saddle can't rewrite past |
| `ci_repair_refused` | `repair_of_repair` | a CI repair task went red itself |
| `stack_stuck` | `stuck_stack` | a red or conflicting layer stayed unfixed past `[train] stuck_after` |
| `train_escalated` | `train_escalated` | a branch failed to land too many times |

The first sweep starts at the newest event, so older history isn't filed. If
`gh` fails, the sweep stops at that event and retries it on the next cycle.
Other code (such as the stall budget) can file a blocker directly with
`BlockerFiler.FileBlocker`.

## Signatures and deduplication

A signature is the blocker kind plus a normalized cause, for example
`ci_red:gitguardian_security_checks` or `train_escalated:tests_failed`.
Normalizing lowercases the cause and drops URLs, commit SHAs, PR and issue
numbers, task IDs and other numbers, then keeps the first six words of each
colon-separated part. Two escalations that differ only in SHA, PR or task get
the same signature. A different check gives a different one.

The signature and count live in the issue body as HTML comments:

```
<!-- saddle-blocker-signature: history_check:gitguardian_security_checks -->
<!-- saddle-blocker-count: 3 -->
```

That means deduplication survives a `state.db` reset. A repeat comments
("Seen again 2026-10-12, t171, #412 (3 times so far).") and rewrites the
count. A closed issue gets reopened instead of duplicated.

## The issue

- Title: the class and the subject, e.g. `History check: GitGuardian Security Checks red on t1's unmerged layer`.
- Labels: `saddle-blocker` and `class:<kind>`. Saddle creates them if they're missing.
- Body: the model-scope line (Opus), a one-sentence summary, what saddle told
  the orchestrator, a timeline built from the task's recent events, the hand
  fix, links to the task, PR and run, and a "Design a fix so this never needs a
  human" section that names the likely code area.

Saddle supplies the facts. The narrator (Haiku, within `[narrator]
daily_cap_usd`) writes only the summary sentence. With no API key, a reached
cap or an API error, a template sentence is used and the issue is filed
anyway.

## Hand fixes

```
saddle blockers handfix t3 "sqlite3 .saddle/state.db \"update train set state='queued' where task='t3'\""
```

This records the exact fix against the task. A hand fix recorded before the
blocker is filed goes into the issue body. One recorded after it is posted as
a comment on the next sentinel cycle.

## Listing

`saddle blockers` lists the open `saddle-blocker` issues, the most frequently
seen first, with each one's count, number, signature and title.

## Turning it off

```toml
[issues]
auto_file = false
```

Filing is on by default, and only in repos you've trusted (`saddle trust`).
While it's off, the sweep still advances its cursor, so turning it back on
doesn't file a backlog. The sweep's cursor lives in `.saddle/blocker-issues.json`.
