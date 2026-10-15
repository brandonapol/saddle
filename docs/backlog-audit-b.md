# Backlog audit B: issues #48–#151

Re-audit on 2026-10-14 against `main` at 5508efd. It covers 26 open issues: incident bugs, merge, TUI and config.

Method:
- Read each issue body with `gh issue view`.
- Confirm the code is on main with grep and `git log`.
- Confirm each named test exists and passes: `go test -run` over the cited tests in `internal/{app,tmux,mcpserver,config,tui,planner,sentinel,cli,agent,sweeper}`, all `ok`.
- Close each DELIVERED issue with an evidence comment.

Earlier audits (`docs/backlog-audit.md`, 2026-10-02, and `docs/audit-open-issues.md`, 2026-10-13) were read-only. This one closes issues.

**26 issues: 23 delivered and closed, 0 partial, 3 open.**

## Delivered (closed)

| Issue | Title | Evidence (PR / file / test) |
|---|---|---|
| #52 | Stacked PRs by default | 33d4a8e (#145) `internal/app/cluster_stack.go`; `train.output` = stack\|single\|per-task, default stack; `Closes #N` in `prtemplate.go`; stack nav in `stack_comments.go`. TestRestackKeepsClusteredLayout |
| #53 | Restack on change, retarget on merge | `app/stack.go`, `stack_github.go`, `internal/sweeper`. TestRestackAfterSquashMerge, TestRestackConflictReturnsToOwner, TestRestackToolMovesStack, TestSweepStackBottomFirstAndRetarget |
| #58 | planner uses claims.Overlap | `internal/planner/check.go`. TestClaimsOverlapAsPlannerUsesIt |
| #65 | Usage / plan-limit bars | d1468e5 (#127) `tui/usage.go`. TestFooterCarriesUsageStrip, TestUsageStripShowsEachWindowAgainstItsCap (per-model breakdown is #41) |
| #67 | Shortcut footer, peek navigation | 509a22b (#133) `tui/keys.go`. TestFooterHelpMatchesBindings, TestPeekTitleShowsAgentPosition, TestCycleAgentSkipsDeadWindows |
| #70 | Wire ciwatch into the app | `app/ci.go`, started from `saddle up`. `app/ci_test.go` (9 tests incl. TestCIFailureOnLandedTaskSpawnsOneFixTask), TestCIDefaults |
| #72 | Urgent red summary | 509a22b (#133) `tui/urgent.go`. TestUrgentRendersRedSummaryWhiteDetails |
| #80 | prs fails after bottom PR merged (incident) | ec1aa0c (#129) ReconcileStack skips merged/closed PRs. Regression: TestRestackSkipsClosedAndMergedPRs, TestRestackToleratesRetargetOfClosedPR |
| #83 | Epic: stack integrity | Children #80, #85–#87, #91–#93, #123 all closed. TestReplayStackIncident, TestReplayGitHubHumansIncident |
| #85 | No supported restack (incident) | bcc7db3 (#101). Regression: TestRestackAfterSquashMerge, TestRestackConflictReturnsToOwner, TestRestackToolMovesStack |
| #86 | Branches drift from landed SHA (incident) | bcc7db3 (#101), `internal/refguard`. Regression: TestPRsPushesLandedSHAs, TestPRsRefusesDriftedBranch, TestWarningsReportDrift |
| #87 | prs doesn't validate stack (incident) | bcc7db3 (#101), sentinel. Regression: TestPRsRefusesForkedStack, TestSentinelFlagsForkedStackUntilRestack, TestFlaggedStackFreezesPRsAndLand |
| #91 | Phantom killed tasks on failed spawn (incident) | e34ec49 (#100), 7b6931f (#306). Regression: TestFailedSpawnIsRecorded covers every bullet of the issue's spec: status `failed`, `spawn_failed` event, no branch or claims, retry hint, no row on claim conflict |
| #92 | Killed tasks leave worktrees (incident) | e34ec49 (#100); `saddle gc`; `saddle down` prints the leftover count (`cli/root.go` downCmd); doctor `leftovers` check. Regression: TestKillCleansUp (no commits, with commits, `--keep`), TestGCRemovesLeftovers |
| #93 | Wake-ups typed into TUI window (incident) | e34ec49 (#100). Regression: TestOrchestratorGetsNoWakeKeys (Notify sends no keys, Orchestrator() clears window), TestNoKeysIntoForeignWindow |
| #108 | Embedded terminal pane | `internal/termpane`, `tui/terminal.go`. `tui/terminal_test.go` |
| #109 | Refuse while merge commits allowed | `app/repocheck.go`. TestCheckRepoMergeSettingsCombinations and the other repocheck tests |
| #110 | tmux mouse copy | `tmux/tmux.go` ConfigureSession (wl-copy, OSC52 fallback). TestConfigureSession* |
| #123 | Batch land drops task (incident) | ec1aa0c (#129). Regression: TestBatchLandKeepsEveryTask, TestRestackRecoversBatchLandedLegacyNotes, TestRestackRestoresTaskDroppedAsMerged, TestReplayGitHubHumansIncident |
| #142 | adapters.<name> and spawn caps config | 0783707 (#157). TestAdaptersAndSpawnCaps, TestSpawnCapsFromConfig |
| #146 | MCP queue tools | 0783707 (#157) `mcpserver/server.go` queue_move/hold/release. TestQueueTools |
| #150 | `saddle up <agent>` | 1f9658e (#301) parseUpArgs; codex is now a harness. TestParseUpArgs, TestUpAgentSetsHarnessEnv, TestLaunchCarriesSessionHarness |
| #151 | Epic: MVP | #147 closed; auto-merge 0783707 (#157, `app/automerge_test.go`); #142/#146 closed; `internal/doctor` + `docs/QUICKSTART.md`; daemon descoped in `docs/ARCHITECTURE.md`; hand-fix rule in AGENTS.md |

None of the incident bugs (#80, #85–#87, #91–#93, #123) still reproduces. Each has a named regression test that passes on main.

## Open (left open, nothing filed)

| Issue | Status | Scope | Difficulty |
|---|---|---|---|
| #48 | OPEN | Dispatcher agent for gray-zone forecasts and hold deadlocks. It returns a structured hold/proceed/sequence/ask_owner/escalate decision, with a rate limit, a daily cost cap and narrator output. Prerequisite now exists: forecaster `internal/forecast` (#139) and the collision feed in `app/forecast.go` (#596). No hold/sequence actuator exists yet. | hard (design judgment: cost cap, prompt shape, what acts on the decision) |
| #51 | OPEN | TUI roster columns for ahead/behind, dirty count and last-commit age; a forecast pairs list (the data is now in `App` via #596, but only `status`/brief show it); `w` explain; ⏸ hold glyph. The hold glyph depends on #48 or some hold concept. | standard (roster columns and forecast list; the hold glyph waits on #48) |
| #78 | OPEN | ADR-first rule: an orchestrator brief rule that epic-level work starts with `docs/adr/NNNN-*.md` (or an epic comment). Worker briefs link the ADR and stop to message the orchestrator if it is wrong. No `docs/adr/` and no rule in `internal/app/briefs.go`. | mechanical (brief text, a template and a brief test) |

## Owner decisions

- **#5 (saddled daemon)** is still open, but `docs/ARCHITECTURE.md` already records the decision: there is no daemon in the MVP. Close #5, or say if the daemon is wanted after the MVP.
- **#78**: confirm the ADR-first rule is wanted. It adds a step before every epic.
- **#48**: confirm an LLM dispatcher is still wanted now that the deterministic forecaster scores same-file, disjoint-symbol pairs low (#596). It may be enough on its own.
