# Audit: landed tasks the train marked superseded or folded

Date: 2026-10-13 (task t225). Read-only audit of `.saddle/state.db`.

t177 (#337) was marked `landed` with train state `superseded`, but its commit
existed only on its own branch until t223 re-applied it. This audit checks every
other task with `tasks.status = 'landed'` and `train.state` in (`superseded`,
`folded`). It asks one question: did the task's own commits reach `origin/main`
(at `54fd85e`) or, failing that, `saddle/integration` (at `9fd6134`)?

## Method

- Commits: the train note's `base..head` range. When the note holds only one
  SHA, the task's `done` summary and its branch decide which commits are its own.
- `git cherry` marks every commit `+`, because the train squash-merges. It
  can't tell present work from lost work, so it was used only to list commits.
- Presence check 1: reverse-apply each commit's diff on an `origin/main` checkout.
- Presence check 2: for each file the commit touched, count how many of its
  added lines (12 or more characters, whitespace trimmed) appear in that
  file's `origin/main` version.
- Presence check 3: grep `origin/main` for the key functions or tests named in
  the commit.

## Results

| Task | Train | Issue | Branch | Commits | Result | Evidence |
|------|-------|-------|--------|---------|--------|----------|
| t13 | superseded | #97 | saddle/t13-tui-require-ctrl-c-twice-to-quit | 6c540d4 | ok | Same subject on main as `3cbcd2e` (#111). 60/66 added lines present; `internal/tui/quit_test.go` is on main. |
| t55 | superseded | — | saddle/t55-rebase-and-land-the-grok-cli-har | deece29, 48e830c, db115b6 | ok | Re-landed as `3c9c7c0` (#174). `internal/agent/grok*.go` and the bridge are on main, with nearly all added lines. |
| t61 | superseded | #178 | saddle/t61-e2e-harness-fake-agent-fake-gith | 823c7ab … 8b1d234 (8) | ok | All `internal/e2e` files are present. The Makefile `test/e2e` target is on main in an evolved form (`cb5c7cf`, #319). |
| t65 | superseded | — | saddle/t65-fix-ci-for-t61-e2e-harness-fake | caec9fe | ok | The fakeagent printf fix is present in `internal/e2e/fakeagent`. |
| t111 | folded | — | saddle/t111-fix-ci-for-t109-heavy-run-schedu | dbe9378 | ok | The diff reverse-applies cleanly on main (folded into `4b5f1b9`, #278). |
| t123 | folded | #240 | saddle/t123-fix-ci-for-t119-heavy-run-schedu | 84e977e | ok | The diff reverse-applies cleanly on main (folded into `e0e9767`, #291). |
| t135 | folded | — | saddle/t135-fix-ci-for-t131-adapters-grok-fl | 10a92c6 | ok | The diff reverse-applies cleanly on main (folded into `1f9658e`, #301). |
| t136 | superseded | — | saddle/t136-fix-ci-for-t135-fix-ci-for-t131 | none of its own | ok | `done` said no code change was needed (stale stack base). `App.AdapterStatus` is on main. |
| t154 | folded | — | saddle/t154-fix-ci-for-t150-remote-control-s | 1491386 | ok | The diff reverse-applies cleanly on main (folded into `dc10b47`, #366). |
| t162 | folded | — | saddle/t162-fix-ci-for-t150-remote-control-s | fdabd8c | ok | The diff reverse-applies cleanly on main (folded into `dc10b47`, #366). |
| t167 | superseded | #317 | saddle/t167-fix-ci-for-t163-remote-control-e | none of its own | ok | `done` said no code change was needed (base lacked the lower remote layers). `remote.Snapshot` and `ObjConcurrency` are on main. |
| t177 | superseded | #337 | saddle/t177-restack-must-not-respawn-repair | a7cb8c2 | ok (pending) | Re-applied by t223 as `9fd6134`. That commit is on `saddle/integration` but not yet on `origin/main`. |
| t177 | superseded | #338 | saddle/t177-restack-must-not-respawn-repair | b2cbfc5 | **missing** | `ciRepairScreen`, `prMetaRe` and `stackBaseMissing` are absent from both main and integration. Filed as #461. |
| t216 | superseded | #250 | saddle/t216-config-document-new-train-tmpdir | none of its own | ok | `done` said the work had already landed. `[train] tmpdir` is documented in `internal/config/config.go` on main. |

## Not audited

Tasks t14 through t18 also have train state `superseded`, but their task status
is `killed`, not `landed`, so they fall outside this audit.
