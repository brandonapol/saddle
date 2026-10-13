# Open-issue audit

Audit of the 125 open GitHub issues against `main` (2026-10-13). Read-only: no issue was closed and no code was changed. For each issue marked delivered or partial a comment was posted on the issue; the owner closes the delivered ones. This supersedes the 2026-10-02 `docs/backlog-audit.md`, which is kept for its batching notes.

Evidence is a merged PR or commit on `main`, or code that exists on `main`, plus a test that covers it. Epics are rated by their children. "Delivered" means the issue's stated scope is in the tree; where a doc-only change has no test, the row says so.

**delivered: 99**  
**partial: 15**  
**open: 11**  

| Issue | Status | Evidence |
|---|---|---|
| #453 | delivered | ec546a1 (landTwo spawns t2 only after t1 is queued); test: internal/e2e/journeys_test.go landTwo helper |
| #452 | partial | done refuses a branch with no commits (internal/app/done.go:159); no 'noop' task state |
| #436 | delivered | a3d80ad (#457) parseRaw quits once the reader drains; test: TestParseRawWaitsForSlowReader |
| #434 | delivered | 8344e14 (#449) orchestrator brief points at docs/GLOSSARY.md; test: TestOrchestratorBriefsPointAtGlossary |
| #433 | delivered | 8ec0076 (#454) prs defers a layer on GH006; test: TestPRsDefersLayerInMergeQueue |
| #432 | delivered | 75d5f0a (#448) watchdog names a hung gate; test: TestWatchdogNamesHungGateOverOverload |
| #431 | delivered | 6f872d1 (#446) restack defers a layer on GH006; test: TestRestackDefersLayerInMergeQueue |
| #430 | delivered | 38d9b93 (#447) gate retries once on a timeout-only failure; test: TestLintDoneRetriesATimeoutOnce |
| #429 | delivered | 1782267 (#443) refresh no longer shows an in-flight auto-merge result; test: TestMergeHeaderWaitsForTheActionToLand |
| #426 | delivered | 18fbe5b (#441) /saddle catchup in the plugin; test: plugin/hooks/catchup.test.ts |
| #425 | delivered | 90bc403 (#440) load-tolerant plugin and hook tests; test: TestDenyQuotesOwnersDiffWhenGitIsSlow, plugin/tests/*.test.ts |
| #415 | delivered | 9d28747 (#428) stall_routed commented onto the blocker issue; test: TestFileBlockersCommentsStallRouted |
| #414 | delivered | c633c55 (#421) repo name pill in the TUI header; test: TestHeaderRepoPill |
| #413 | delivered | 5c5e2f1 (#442) a landing entry moves behind the train; test: TestStackFollowsLandingOrderAfterHoldRelease |
| #412 | delivered | 79ce74b (#439) alt+c catch-me-up and last-looked tracking; test: TestAltCShowsCatchUp |
| #410 | delivered | Epic: all eight children (#402-#409) are delivered, see their rows; test: the children's tests (#402-#409) |
| #409 | delivered | db92a1c (#423) saddle up --rc; test: TestUpRCFlagAndConfig |
| #408 | delivered | 377cd54 (#424) failure-mode catalog and doctor --predict; test: TestUpPredictOpensOneCardWithoutBlocking |
| #407 | delivered | ec38edc (#420) one deduplicated GitHub issue per blockage; test: TestFileBlockerCreatesThenComments |
| #406 | delivered | 11e96fc (#422) stall budget and no-progress watchdog; test: TestStallRouteAroundIndependentLayerMerges |
| #405 | delivered | f8dc380 (#416) history scan before land, rewrite on red history check; test: TestLandRejectsSecretInAnyCommit |
| #404 | delivered | ee3d3c4 (#418) decision cards; test: TestDecisionProducersFillEveryField |
| #403 | delivered | d61e8f6 (#419) state headline and catch-me-up; test: TestCatchUpOrdersNeedsYouFirstAndCapsAtFive |
| #402 | delivered | ec1cba9 (#417) headline first, details folded; test: TestOrchestratorBriefTalkingGolden |
| #395 | delivered | c8ada5a (#445) trainLint takes the repo-hook lock and retries; test: TestTrainLintWaitsOnTheHookLock |
| #385 | delivered | 5cc6bf3 (#437) ARCHITECTURE.md covers the per-task scratch dir (docs only, no test); test: docs/ARCHITECTURE.md scratch section (docs-only change, no test) |
| #359 | delivered | 5659429 (#363) and 81a98f2 (#455) keep manual retargets; test: TestRestackKeepsManualRetargetWhenBaseMoves |
| #338 | partial | Cancelled runs no longer spawn repairs (9503277, #237); no PR-footer suppression |
| #337 | open | No superseded mark or ask_owner before spawning a repair for a landed task |
| #336 | partial | Lint repair folds into a red layer (#218, e2e only); deadlock case not shown fixed |
| #334 | delivered | 740db7b (#450) README vocabulary and docs/GLOSSARY.md, 4e6a68a (#378); test: TestGlossaryCoversEveryCommand |
| #331 | open | No Saddle-Task trailers or identity-based merged check in the tree |
| #330 | open | No Mergiraf or structural merge step anywhere in internal/ or docs |
| #329 | open | internal/gitx tracks symbols, but claims, hook and forecast do not use them |
| #315 | delivered | 04bbda5 (#370) remote doctor check; test: TestRemoteControlCheck |
| #314 | delivered | 04bbda5 (#370) TUI indicator of connected controllers; test: TestParseRemoteEvent |
| #313 | delivered | 645ef8e (#373) remote-safe actions; test: TestActionsNeedAdminAndConfirm |
| #312 | delivered | 645ef8e (#373) multi-repo addressing; test: TestStatusHasVersions |
| #311 | delivered | dc10b47 (#366) push: wait_needs_you; test: TestWaitNeedsYouWakesOnNewItem |
| #310 | delivered | dc10b47 (#366) snapshot and needs-you queue; test: TestNeedsYouHasCursor |
| #309 | delivered | 731a027 (#364) auth, scopes, audit; test: TestAuditRotates |
| #308 | delivered | 731a027 (#364) server and transport hardening; test: TestAuditFollowsRotationByAnotherProcess |
| #292 | partial | 6fe9208 (#427) lets the orchestrator release another task's claim; no handoff path |
| #272 | delivered | ff9f56d (#283) one capture call for all agents; test: TestCaptureManyIsOneCall |
| #266 | delivered | ff9f56d (#283) TUI polish pass; test: TestJourneyTUIPolish |
| #264 | delivered | 9a6c401 (#300) x and L ask first; test: TestKillAndLandAskFirst |
| #263 | delivered | 9a6c401 (#300) blank error fixed; dcbf20e (#305) doctor grok check; test: TestGrokModelAndBlankError |
| #262 | delivered | 9a6c401 (#300) retry notice replaces 'thinking'; test: TestRetryReplacesThinking |
| #261 | delivered | 9a6c401 (#300) /compact and /clear show in chat; test: TestCompactAndClearShowInChat |
| #260 | delivered | ff9f56d (#283) an error result clears busy; test: TestErrorResultClearsBusy |
| #250 | partial | Config comments and field doc updated (config.go:386-390, 765-771); no test pins the template |
| #247 | partial | t84 collapse delivered (c8710c2 #210); t83 and t85 not verified |
| #207 | delivered | 6fba59e (#233) and 65d321c; test: TestMergeKeysToggleAgainstFreshState |
| #201 | delivered | 6fba59e (#233) scrubber releases alt+[; test: TestMouseScrubReleasesAltBracket |
| #200 | delivered | 6fba59e (#233) split alt+` joins; test: TestSplitAltBacktickTogglesTerminal |
| #190 | delivered | 640f670 (#195) prs after unstack plus restack; test: TestJourneyPRsRightAfterUnstackRestack |
| #184 | delivered | 9503277 (#237) gate retries environment failures; test: TestRunGateEnvRetriesEnvironmentFailure |
| #182 | delivered | 1f9658e (#301) mixed workers; test: TestLaunchCarriesSessionHarness |
| #181 | delivered | 1f9658e (#301) grok flags; test: TestGrokLaunchRunsUnderInstalledGrok |
| #165 | delivered | plugin mod: snapshot, pane, needs-you band, commands, wake, claim guard (all six sub-items); test: plugin/tests/snapshot.test.ts, pane.test.ts, needsyou.test.ts, commands.test.ts, wake.test.ts, claimguard.test.ts |
| #151 | delivered | Epic done-when items: #147 closed, auto-merge 0783707 (#157), #142/#146, doctor, daemon decision (ARCHITECTURE.md:17); test: the children's tests (see #142, #146 and the auto-merge tests in internal/automerge) |
| #150 | delivered | 1f9658e (#301) saddle up chooses the agent; test: TestLaunchCarriesSessionHarness |
| #146 | delivered | 0783707 (#157) queue_move, queue_hold, queue_release MCP tools; test: TestQueueTools |
| #142 | delivered | 0783707 (#157) adapters.<name> and spawn caps in config; test: TestAdaptersAndSpawnCaps and TestSpawnCapsFromConfig |
| #123 | delivered | ec1aa0c (#129) batch land keeps every task; test: TestBatchLandKeepsEveryTask |
| #110 | delivered | Session-scoped mouse and copy binding (internal/tmux/tmux.go); test: TestConfigureSessionScopedMouse |
| #109 | delivered | internal/app/repocheck.go refuses allow_merge_commit; test: TestCheckRepoMergeSettingsCombinations |
| #108 | delivered | internal/termpane and internal/tui/terminal.go; test: internal/tui/terminal_test.go |
| #93 | delivered | e34ec49 (#100) no wake keys into foreign windows; test: TestOrchestratorGetsNoWakeKeys |
| #92 | delivered | e34ec49 (#100) kill cleans worktree and branch; test: TestKillCleansUp |
| #91 | delivered | e34ec49 (#100) and 7b6931f (#306) failed spawn keeps its reason; test: TestFailedSpawnIsRecorded |
| #87 | delivered | bcc7db3 (#101) prs validates the stack; test: TestPRsRefusesForkedStack |
| #86 | delivered | bcc7db3 (#101) landed SHAs pushed, drift refused; test: TestPRsRefusesDriftedBranch |
| #85 | delivered | bcc7db3 (#101) restack; test: TestRestackAfterSquashMerge |
| #83 | delivered | Epic: children #85-#87, #91-#93, #123 delivered (see rows); test: the children's tests (#85-#87, #91-#93, #123) |
| #80 | delivered | ReconcileStack skips merged and closed PRs; test: TestRestackSkipsClosedAndMergedPRs |
| #78 | open | No ADR convention in briefs.go and no docs/adr |
| #72 | delivered | 509a22b (#133) urgent summaries; test: TestUrgentRendersRedSummaryWhiteDetails |
| #70 | delivered | internal/app/ci.go wires ciwatch into the app; test: internal/app/ci_test.go |
| #67 | delivered | 509a22b (#133) shortcut footer; test: TestFooterHelpMatchesBindings |
| #65 | delivered | d1468e5 (#127) usage strip with plan-limit bars; test: TestFooterCarriesUsageStrip |
| #58 | delivered | internal/planner/check.go calls claims.Overlap; test: TestClaimsOverlapAsPlannerUsesIt |
| #53 | delivered | restack and retarget in internal/app/stack.go; test: TestRestackAfterSquashMerge |
| #52 | delivered | 33d4a8e (#145) and internal/app/cluster_stack.go clustered stacks; test: TestRestackKeepsClusteredLayout |
| #51 | open | No ahead/behind, dirty or forecast columns in the TUI |
| #48 | open | No dispatcher agent in the tree |
| #45 | open | No reactor; the app decides inline |
| #44 | open | internal/forecast exists but is not wired; no reactor, dispatcher or holds |
| #43 | delivered | 1b16efa (#148) ask the narrator; test: TestAskNarratorThreadsQuestionAndAnswer |
| #41 | delivered | 1b16efa (#148) usage graph; test: TestBuildUsageGraph |
| #38 | delivered | Epic: children #41 and #43 delivered (rest already closed); test: TestBuildUsageGraph, TestAskNarratorThreadsQuestionAndAnswer |
| #36 | delivered | 74f3905 (#173) merge train view; test: TestMergeViewShowsStacks |
| #34 | delivered | 1b16efa (#148) control view; test: TestJourneyTUIViewRouter |
| #33 | delivered | 1b16efa (#148) shell, keymap and view router; test: TestViewSwitchKeys |
| #32 | delivered | Epic: #33-#37 delivered or closed (see rows); test: TestViewSwitchKeys, TestMergeViewShowsStacks |
| #31 | open | No trial rebase of queued branches |
| #30 | delivered | 33d4a8e (#145) escalation after max attempts; test: TestConflictEscalatesAfterMaxAttempts |
| #29 | open | No structural merge driver evaluated or wired |
| #28 | delivered | 33d4a8e (#145) regen globs; test: TestLandRegeneratesDerivedFileOnConflict |
| #27 | partial | Planner marks barriers; no runtime lane pause |
| #26 | partial | Rename map and claim remap landed; worktrees rely on saddle sync |
| #25 | delivered | Serial train plus queue hold, move, release (33d4a8e #145, 0783707 #157); test: TestQueueTools |
| #24 | partial | Epic: #27, #29, #31 not delivered |
| #23 | delivered | notices table, app.Notify, safe send; test: TestOrchestratorGetsNoWakeKeys |
| #22 | delivered | MCP tools incl. brief and ask_owner, spawn caps (0783707 #157); test: TestBriefTool |
| #21 | delivered | internal/hook/hook.go; test: internal/hook/hook_test.go |
| #20 | delivered | internal/claims/claims.go; test: TestClaimsOverlapAsPlannerUsesIt and internal/claims tests |
| #19 | delivered | Epic: #20-#23 delivered; test: the children's tests |
| #18 | partial | Needs-you detection and idle wake exist; status segment coverage not shown |
| #17 | partial | Concurrency cap and plan-limit pause; no wave gating or pause/resume |
| #16 | delivered | 1f9658e (#301) codex and grok adapters; test: TestCodexLaunchWiresMCPAndBrief |
| #15 | delivered | internal/agent/adapter.go Adapter interface; test: TestByName |
| #14 | delivered | saddle status --tmux segment and session-scoped copy config; test: TestTmuxSegmentFormat |
| #13 | delivered | worktree manager in internal/app/lifecycle.go; test: TestKillCleansUp |
| #12 | partial | Epic: #17 not delivered |
| #11 | delivered | ace9a19 (#143) push plan as issues (internal/planner/push.go); test: internal/planner/push_test.go |
| #10 | delivered | ace9a19 (#143) plan edit and approve; test: TestEditRechecks and TestApproveFreezesBase |
| #8 | delivered | ace9a19 (#143) structured planner schema; test: internal/planner/schema_test.go |
| #7 | delivered | ace9a19 (#143) epic sources including gh: refs; test: internal/planner/source_test.go |
| #6 | delivered | Epic: #7, #8, #10, #11 delivered; test: the children's tests |
| #5 | delivered | Descoped: docs/ARCHITECTURE.md:17 records the no-daemon decision; test: docs/ARCHITECTURE.md:17 (decision, no code) |
| #4 | partial | Store tables exist; no epics or deps tables |
| #3 | partial | Global and per-repo TOML with regen and adapters keys; no models.* keys |
| #2 | delivered | cmd/saddle, Cobra CLI, CI workflow; test: internal/cli/root_test.go TestVersion |
| #1 | partial | Epic: #3 and #4 partial |
