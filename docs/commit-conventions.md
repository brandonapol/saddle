# Commit subjects and PR titles

Saddle preserves workers' commit messages through landing and restacking. It
uses the task title as the PR title, so give `spawn --title` the repository's
normal title format, for example `fix(storage): report capacity correctly`.
Worker briefs require the repository's conventions and forbid a `saddle: `
subject prefix or tool attribution. Tracking belongs outside the subject.
