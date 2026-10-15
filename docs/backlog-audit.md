# Backlog audit

Re-audit date 2026-10-14 against `main` at 5508efd. Scope: the issues still open in #1-#45, including the epics #1, #6, #12, #19, #24, #32, #38 and #44. This replaces the 2026-10-02 audit of this file. Issues above #45 are covered by `docs/audit-open-issues.md`.

Each DELIVERED issue got a comment with its evidence and was closed. Each issue left open got a comment that lists what is missing. An epic was closed only when every child was delivered or closed.

**39 open issues audited: 29 closed (23 delivered, 2 superseded or descoped, 4 epics complete), 10 left open (4 partial, 2 open, 4 epics).**

Owner decisions (2026-10-14):
- **#45 reactor: descoped and closed.** Saddle stays in-process (#5); scheduling decisions stay inline in `internal/app`. The `docs/ARCHITECTURE.md` edit that records this was handed to t298, which holds that file.
- **#4 plan tables: kept open.** #17's dependency gating needs `epics` and `deps` persisted.

| Issue | Title | Status | Evidence | Missing |
|---|---|---|---|---|
| #1 | Epic: Foundation | OPEN (epic) | #2 delivered; #5 superseded | #3, #4 partial |
| #2 | Scaffold Go module, CLI skeleton and CI | CLOSED: delivered | `cmd/saddle`, `internal/cli/root.go`, `.github/workflows/ci.yml`; TestVersion | |
| #3 | Config: global + per-repo TOML | PARTIAL | `internal/config/config.go`: merge, `regen`, `adapters.<name>` (cmd, model, models per difficulty), `narrator.model`, `claude.orchestrator_model`; `config_test.go` | `saddle config` printing the merged config; planner model key (`planner/model.go` hard-codes DefaultModel) |
| #4 | State store schema (SQLite) | PARTIAL | `internal/store/store.go` migrations: tasks, claims, events, train, notices, renames, chat, usage, narrator_spend, triage_calls | `epics`, `deps` tables, claim `kind`. Kept open: #17 needs it |
| #5 | Daemon `saddled` | CLOSED: superseded | `docs/ARCHITECTURE.md` "Processes": in-process, no daemon (decision #5) | |
| #6 | Epic: Planner | CLOSED: complete | #7, #8, #10, #11 delivered (#143); #9 closed | |
| #7 | Epic sources | CLOSED: delivered | #143; `internal/planner/source.go` (file, stdin, `gh:#N`); `source_test.go` | |
| #8 | Planner prompt + structured output | CLOSED: delivered | #143; `planner/model.go`, `prompt.go`, `schema.go`, `snapshot.go`; `schema_test.go` | |
| #10 | Plan review: edit, approve | CLOSED: delivered | #143; `planner/review.go`; TestEditRechecks, TestApproveFreezesBase | |
| #11 | Push plan as GitHub issues | CLOSED: delivered | #143; `planner/push.go`; `push_test.go` | |
| #12 | Epic: Runner | OPEN (epic) | #13-#16, #18 delivered | #17 partial |
| #13 | Worktree manager | CLOSED: delivered | `app/lifecycle.go`; TestKillCleansUp, TestGCRemovesLeftovers, TestFailedSpawnIsRecorded | |
| #14 | tmux controller | CLOSED: delivered | `internal/tmux`, `status --tmux`; TestTmuxSegmentFormat, TestTmuxSegmentCounts, `copy_test.go`, `safesend_test.go` | |
| #15 | Adapter interface + Claude adapter | CLOSED: delivered | `agent/adapter.go`; TestByName, TestClaudeAdapterLaunchesLikeWrite | |
| #16 | Codex and Grok adapters | CLOSED: delivered | #301, #498, #518, #570; `agent/hookless.go`, `grok.go`, `codex_bridge.go`; TestCodexLaunchWiresMCPAndBrief, `grok_test.go` | |
| #17 | Scheduler | PARTIAL | Cap (TestSpawnRefusedWhenOverCap), plan-limit pause, `saddle down`, `resume <task>`, `autopilot pause\|resume`, kill | Dependency-ready gating (needs #4), barrier lane pause (#27) |
| #18 | Needs-you detection | CLOSED: delivered | `hook/hook.go:87`, `app/orphan.go` answerPrompt, status segment, TUI headline, narrator; TestTmuxSegmentCounts, TestDetectPrompt, TestHeadlinePriority, TestNeedsYouLinesAreHighlighted | |
| #19 | Epic: Coordination | CLOSED: complete | #20-#23 delivered | |
| #20 | Claims engine | CLOSED: delivered | `internal/claims`, `app.CheckWrite`; TestPreToolUseDeniesOtherTasksFiles, TestClaimsOverlapAsPlannerUsesIt | |
| #21 | Hook receiver | CLOSED: delivered | `internal/hook/hook.go`; TestPreToolUseDeniesOtherTasksFiles, TestNoticesDeliveredAndStopBlocks | |
| #22 | MCP server | CLOSED: delivered | `internal/mcpserver` incl. brief, ask_owner; `app/spawncaps.go`; TestBriefTool, TestAskOwnerTool, TestSpawnDepthCap, TestSpawnFanOutCap | |
| #23 | Mailbox | CLOSED: delivered | notices, `app.Notify`, safe send; TestOrchestratorGetsNoWakeKeys, TestNoKeysIntoForeignWindow | |
| #24 | Epic: Merge train | OPEN (epic) | #25, #26, #28, #30 delivered | #27 partial; #29, #31 open |
| #25 | Serial train | CLOSED: delivered | `app/train.go`, queue hold/move/release (#145, #157); TestQueueTools | |
| #26 | Rename map | CLOSED: delivered | #145; `app/sync.go` autoRebase; TestLandBroadcastsRenamesAndRemapsClaims, TestLandAutoRebasesLiveWorktrees, TestAutoRebaseCanBeTurnedOff | |
| #27 | Barrier tasks | PARTIAL | `planner/check.go` marks barriers in wave 0; brief rule `app/briefs.go:111` | Runtime lane pause, land alone, resume |
| #28 | Derived-file regeneration | CLOSED: delivered | #145; `regen` config; TestLandRegeneratesDerivedFileOnConflict | |
| #29 | Structural merge driver | OPEN | Nothing | Evaluate mergiraf/Weave on recorded conflicts, pick one |
| #30 | Return-to-producer + escalation | CLOSED: delivered | #145; TestConflictEscalatesAfterMaxAttempts, TestConflictReturnsToProducer | |
| #31 | Continuous pre-rebase | OPEN | Nothing | Trial-rebase queued branches onto the projected head |
| #32 | Epic: TUI | CLOSED: complete | #33, #34, #36 delivered; #35, #37 closed | |
| #33 | App shell, view router | CLOSED: delivered | #148; `tui/views.go`; TestViewSwitchKeys, TestJourneyTUIViewRouter | |
| #34 | Control view | CLOSED: delivered | #148; `tui/control.go`; `control_test.go` | |
| #36 | Merge train view | CLOSED: delivered | #173; TestMergeViewShowsStacks | |
| #38 | Epic: Narrator + usage | CLOSED: complete | #41, #43 delivered; #39, #40, #42 closed | |
| #41 | Usage graph | CLOSED: delivered | #148; `tui/graph.go`; TestBuildUsageGraph | |
| #43 | Ask the narrator | CLOSED: delivered | #148; `tui/ask.go`; TestAskNarratorThreadsQuestionAndAnswer | |
| #44 | Epic: Event loop + dispatcher | OPEN (epic) | #46, #47, #49, #50 closed; #45 descoped; forecast feed in status and brief (#596) | #48, #51 open |
| #45 | Reactor | CLOSED: descoped | Owner decision: no reactor; decisions stay inline in `internal/app` | |

## Remaining work

- **Small:** #3 (`saddle config` command, planner model key).
- **Needs #4 first:** #17 dependency gating, then #27 barrier lane pause.
- **Merge train:** #31 pre-rebase (M), #29 structural merge spike (M).
- **#4 plan tables:** prerequisite for #17.
