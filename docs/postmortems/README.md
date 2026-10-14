# Postmortems

SRE mode (#547) writes one postmortem per incident to
`docs/postmortems/<date>-<slug>.md`. The date is the incident start (UTC) and
the slug is the lowercased title. An existing file is never overwritten: a
second incident with the same name gets `-2`, `-3`, and so on.

## Format

Each file has these sections, in order:

1. **Header**: start, end (or "ongoing") and duration.
2. **Timeline**: dated events, sorted oldest first.
3. **Lane findings**: what each investigation lane reported. Omitted when
   there are none.
4. **Root cause**: "Unknown" if none was confirmed.
5. **Verification**: which different agent confirmed the cause, and how.
6. **Fix**: the PRs that fixed it.
7. **Follow-ups**: work still to do.
8. **Proposed runbook additions**: for each `.saddle/sre.toml` check that found
   the cause, its description, read-only command or surface, and expected
   result. Omitted when no check found the cause.

Credentials are never copied into a postmortem; only check names and commands.

## Runbook additions are proposals

No runbook exists yet. The writer never edits one. A human (or a later,
reviewed change) copies useful proposals from a postmortem into a runbook, so
the runbook grows from real incidents, seeded by the config's checks.
