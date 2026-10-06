// Package config loads saddle's TOML config: defaults, then
// ~/.config/saddle/config.toml, then <repo>/.saddle/config.toml.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/brandonapol/saddle/internal/usage"
)

type Config struct {
	// Base is the branch the epic starts from and the bottom of the PR stack.
	Base string `toml:"base"`
	// Integration is the branch the merge train lands onto, one task at a time.
	Integration string `toml:"integration"`
	// Session is the tmux session name. Empty means "saddle-<repo dir name>".
	Session string `toml:"session"`
	// Concurrency caps how many worker agents run at once.
	Concurrency int `toml:"concurrency"`
	// Serial globs are files only the merge train should change (lockfiles, migrations).
	Serial []string `toml:"serial"`
	// CloseOnLand kills a task's tmux window and removes its worktree once it lands.
	CloseOnLand bool `toml:"close_on_land"`
	// Regen lists derived files (go.sum, lockfiles, generated code) the merge
	// train regenerates instead of text-merging when they conflict.
	Regen []Regen `toml:"regen"`

	Test  Test  `toml:"test"`
	Train Train `toml:"train"`
	// Harness is which coding CLI workers and the orchestrator run.
	// "claude" (default) or "grok".
	Harness string `toml:"harness"`
	Claude  Claude `toml:"claude"`
	Grok    Grok   `toml:"grok"`
	Triage  Triage `toml:"triage"`
	Usage   Usage  `toml:"usage"`
	// Limits are the plan caps behind the usage strip's ok/warn/over states,
	// and, with pause_launches, a hold on new spawns once a window is over.
	Limits usage.Limits `toml:"limits"`
	// Narrator summarizes agent events into the chat thread with a cheap
	// model. Off unless ANTHROPIC_API_KEY and daily_cap_usd are both set.
	Narrator Narrator `toml:"narrator"`
	// Sweeper merges ready saddle PRs. Off unless enabled.
	Sweeper Sweeper `toml:"sweeper"`
	// CI watches saddle PRs' checks while saddle up runs.
	CI CI `toml:"ci"`
	// Adapters sets the command and extra arguments of agent CLIs other than
	// Claude, by adapter name (codex, grok).
	Adapters map[string]Adapter `toml:"adapters"`
	// Spawn caps how deep and wide spawn chains below the orchestrator go.
	Spawn Spawn `toml:"spawn"`
	// Orchestrator tunes the orchestrator session itself.
	Orchestrator Orchestrator `toml:"orchestrator"`
	// Notices tunes how queued notices reach idle agents.
	Notices Notices `toml:"notices"`
}

// Notices configures notice delivery to agents.
type Notices struct {
	// WakeAfter is how long an idle agent may sit with undelivered action
	// notices before saddle wakes it even over what looks like a draft (#183).
	WakeAfter time.Duration `toml:"wake_after"`
	// DigestEvery is how often routine orchestrator notices (landed, merged,
	// restacked, CI green) are rolled into one digest line instead of one
	// message each (#222).
	DigestEvery time.Duration `toml:"digest_every"`
}

// Orchestrator configures the orchestrator session.
type Orchestrator struct {
	// CompactAt is the fraction of its context window at which the
	// orchestrator is told to compact, and compacted once it is idle.
	CompactAt float64 `toml:"compact_at"`
}

// Adapter is how saddle launches an agent CLI. An empty Cmd means the
// adapter's default binary.
type Adapter struct {
	Cmd  string   `toml:"cmd"`
	Args []string `toml:"args"`
}

// Spawn caps sub-task spawning. 0 means no cap.
type Spawn struct {
	// MaxDepth is how deep spawn chains go: the orchestrator's tasks are
	// depth 1, their sub-tasks depth 2, and so on.
	MaxDepth int `toml:"max_depth"`
	// MaxChildren caps a task's working children. The orchestrator's are
	// capped by concurrency instead.
	MaxChildren int `toml:"max_children"`
}

// Regen is a set of derived files and the command that rebuilds them. When a
// rebase in the train conflicts only in files matching Paths (claim globs),
// the train takes integration's copy, runs Cmd in the task's worktree and
// commits what it changed.
type Regen struct {
	Paths []string `toml:"paths"`
	Cmd   string   `toml:"cmd"`
}

// CI configures the CI watcher, which tells the owning task and the
// orchestrator when a saddle PR's checks fail, and spawns a fix task when the
// owner has already landed.
type CI struct {
	// Interval is how often PR checks are polled.
	Interval time.Duration `toml:"interval"`
	// Disabled turns the watcher off.
	Disabled bool `toml:"disabled"`
	// RedInterval is how often the ci-red watcher polls stacked PRs (one gh
	// call each) while something is red or changing; RedMaxInterval caps
	// its backoff while all is quiet.
	RedInterval    time.Duration `toml:"red_interval"`
	RedMaxInterval time.Duration `toml:"red_max_interval"`
	// RepairAttempts is how many repair tasks a red layer gets before the
	// orchestrator is asked to step in.
	RepairAttempts int `toml:"repair_attempts"`
	// Flaky are known flaky checks, as "workflow / job" labels or bare job
	// names. A failure of one is rerun once before it counts as red (#234).
	Flaky []string `toml:"flaky"`
}

// Sweeper configures `saddle sweep`, which merges open saddle PRs that are
// green, mergeable, tested and not flagged for review.
type Sweeper struct {
	// Enabled opts the repo in. Off by default; a dry run works either way.
	Enabled bool `toml:"enabled"`
	// Method is the gh merge method: squash, merge or rebase.
	Method string `toml:"method"`
	// ReviewLabel marks PRs a human must look at. The sweeper never merges a
	// PR that carries it, and applies it to risky PRs.
	ReviewLabel string `toml:"review_label"`
	// DryRun makes every sweep report only, as if --dry-run were passed.
	DryRun bool `toml:"dry_run"`
}

// SweepMethods are the merge methods gh accepts.
var SweepMethods = []string{"squash", "merge", "rebase"}

// Usage configures token metering and the plan-limit bars.
type Usage struct {
	// Poll is how often transcripts are read for new usage.
	Poll time.Duration `toml:"poll"`
	// CountCacheReads includes cache-read tokens in window totals. Off by
	// default: they dwarf everything else and weigh little against plan limits.
	CountCacheReads bool `toml:"count_cache_reads"`
	// Windows are trailing windows shown as bars. Setting any replaces the defaults.
	Windows []Window `toml:"windows"`
}

// Window is a trailing usage window, e.g. the last 5 hours.
type Window struct {
	Name string        `toml:"name"`
	Span time.Duration `toml:"span"`
	// Cap is the token cap for the window; 0 means no cap, so no bar, only a total.
	Cap int64 `toml:"cap"`
}

// Narrator configures the event narrator.
type Narrator struct {
	// DailyCapUSD stops API calls once a day's spend reaches it. 0 turns the
	// narrator off.
	DailyCapUSD float64 `toml:"daily_cap_usd"`
	// Model overrides the narrator's default model (Claude Haiku).
	Model string `toml:"model"`
}

// Triage gates attention with TypeSafe's Jev when TYPESAFE_API_KEY is set.
type Triage struct {
	// Disabled turns Jev triage off even when a key is present.
	Disabled bool `toml:"disabled"`
	// NoAutoApprove keeps routine prompts going to the orchestrator instead of
	// being answered "Yes" automatically.
	NoAutoApprove bool `toml:"no_auto_approve"`
}

// Train configures the merge train.
type Train struct {
	// MaxAttempts is how many failed lands (conflicts, red tests) a branch
	// gets before the train escalates it to you instead of returning it to
	// its producer again.
	MaxAttempts int `toml:"max_attempts"`
	// NoAutoRebase stops the train rebasing live agents' clean worktrees onto
	// integration after each landing; they are only told to sync.
	NoAutoRebase bool `toml:"no_auto_rebase"`
	// Output is how prs lays out PRs: "stack" groups dependent or same-topic
	// tasks into stacks and puts unrelated ones on base; "single" makes one
	// linear stack in train order; "per-task" puts every task on base unless
	// its work only applies on top of an earlier task's.
	Output string `toml:"output"`
	// AutoMerge lets saddle merge the bottom PR of a ready stack itself, then
	// restack, until the stack is empty. Off by default; `saddle automerge
	// on|off` overrides it at runtime and holds keep single stacks out.
	AutoMerge bool `toml:"auto_merge"`
	// StackBackend is how stacks are published: "saddle" chains each PR's
	// base onto the one below it; "gh-stack" also links every stack on
	// GitHub as a native stacked PR with the gh-stack extension, falling
	// back to "saddle" when it is missing or the repo lacks Stacked PRs.
	StackBackend string `toml:"stack_backend"`
	// Lint is the repo's own pre-commit/lint gate, written `lint.cmd`.
	Lint Lint `toml:"lint"`
	// Prepublish is the pre-publish gate, written `prepublish.cmd` and so on.
	Prepublish Prepublish `toml:"prepublish"`
	// StuckAfter is how long a stack may stay red or conflicting with no task
	// fixing it before the orchestrator is interrupted once (#223).
	StuckAfter time.Duration `toml:"stuck_after"`
	// Tmpdir is the disk-backed scratch dir the test gate's TMPDIR and
	// GOTMPDIR point into, relative to the repo root; empty means .saddle/tmp
	// (#184).
	Tmpdir string `toml:"tmpdir"`
}

// Prepublish configures the pre-publish gate (#223). Before prs or publish
// pushes a layer, saddle checks out that layer's own tip in a scratch
// worktree and runs lint.cmd, [test] cmd and Cmd there, bottom to top; a red
// layer and everything above it stay unpublished. Restack runs only Cmd on
// each layer it re-cut, so keep it cheap (spelling, migration numbers).
type Prepublish struct {
	Cmd string `toml:"cmd"`
	// Parallel is how many layers are checked at once; 1 by default.
	Parallel int `toml:"parallel"`
	// Timeout caps each check on each layer; a check that runs over is red.
	Timeout time.Duration `toml:"timeout"`
	// Off turns the gate off: prs publishes without checking each layer.
	Off bool `toml:"off"`
}

// Lint configures the repo's own pre-commit/lint gate (#212). The train runs
// it after [test] cmd on the rebased tree, and done runs it in the worktree;
// red returns the branch to its agent. Unset, saddle detects the gate (the
// repo's pre-commit hook, pre-commit, lefthook, husky, make check/lint);
// set to "" it is off.
type Lint struct {
	Cmd string `toml:"cmd"`
	// Set is whether a config file set cmd, even to "".
	Set bool `toml:"-"`
}

// Disabled reports whether config turned the gate off with lint.cmd = "".
func (l Lint) Disabled() bool { return l.Set && strings.TrimSpace(l.Cmd) == "" }

// Outputs are the PR layouts prs supports.
var Outputs = []string{"stack", "single", "per-task"}

// Stack backends.
const (
	StackBackendSaddle  = "saddle"
	StackBackendGhStack = "gh-stack"
)

// StackBackends are the stack backends prs supports.
var StackBackends = []string{StackBackendSaddle, StackBackendGhStack}

type Test struct {
	// Cmd runs in the task worktree after rebasing onto integration; non-zero blocks landing.
	Cmd string `toml:"cmd"`
}

type Claude struct {
	Cmd               string `toml:"cmd"`
	Model             string `toml:"model"`
	OrchestratorModel string `toml:"orchestrator_model"`
	PermissionMode    string `toml:"permission_mode"`
}

// Grok configures the Grok CLI (`grok`), used when Harness is HarnessGrok.
// An empty model leaves the choice to grok's own default.
type Grok struct {
	Cmd               string `toml:"cmd"`
	Model             string `toml:"model"`
	OrchestratorModel string `toml:"orchestrator_model"`
	PermissionMode    string `toml:"permission_mode"`
}

const (
	HarnessClaude = "claude"
	HarnessGrok   = "grok"
)

func Default() Config {
	return Config{
		Base:        "main",
		Integration: "saddle/integration",
		Concurrency: 5,
		CloseOnLand: true,
		Harness:     HarnessClaude,
		Claude: Claude{
			Cmd:               "claude",
			Model:             "opus",
			OrchestratorModel: "sonnet",
			PermissionMode:    "auto",
		},
		Grok: Grok{
			Cmd:            "grok",
			PermissionMode: "bypassPermissions",
		},
		Sweeper: Sweeper{
			Method:      "squash",
			ReviewLabel: "requires review",
		},
		CI:           CI{Interval: 10 * time.Minute, RedInterval: 2 * time.Minute, RedMaxInterval: 16 * time.Minute, RepairAttempts: 2},
		Spawn:        Spawn{MaxDepth: 3, MaxChildren: 8},
		Orchestrator: Orchestrator{CompactAt: 0.7},
		Notices:      Notices{WakeAfter: 3 * time.Minute, DigestEvery: 15 * time.Minute},
		Train: Train{MaxAttempts: 2, Output: "stack", StackBackend: StackBackendSaddle,
			Prepublish: Prepublish{Parallel: 1, Timeout: 30 * time.Minute}, StuckAfter: 30 * time.Minute},
		Usage: Usage{
			Poll: 15 * time.Second,
			Windows: []Window{
				{Name: "5h", Span: 5 * time.Hour},
				{Name: "7d", Span: 7 * 24 * time.Hour},
			},
		},
	}
}

// Load merges config files over the defaults. Missing files are skipped.
func Load(root string) (Config, error) {
	cfg := Default()
	paths := []string{filepath.Join(root, ".saddle", "config.toml")}
	if home, err := os.UserConfigDir(); err == nil {
		paths = append([]string{filepath.Join(home, "saddle", "config.toml")}, paths...)
	}
	for _, p := range paths {
		// TOML decodes arrays element by element over what is already there, so
		// a file that sets windows would otherwise inherit leftover defaults.
		prev := cfg.Usage.Windows
		cfg.Usage.Windows = nil
		md, err := toml.DecodeFile(p, &cfg)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return cfg, err
		}
		if !md.IsDefined("usage", "windows") {
			cfg.Usage.Windows = prev
		}
		if md.IsDefined("train", "lint", "cmd") {
			cfg.Train.Lint.Set = true
		}
	}
	if cfg.Session == "" {
		cfg.Session = "saddle-" + filepath.Base(root)
	}
	if cfg.Harness == "" {
		cfg.Harness = HarnessClaude
	}
	if cfg.Harness != HarnessClaude && cfg.Harness != HarnessGrok {
		return cfg, fmt.Errorf("harness %q: want %q or %q", cfg.Harness, HarnessClaude, HarnessGrok)
	}
	if cfg.Grok.Cmd == "" {
		cfg.Grok.Cmd = "grok"
	}
	if cfg.Grok.PermissionMode == "" {
		cfg.Grok.PermissionMode = Default().Grok.PermissionMode
	}
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	if cfg.Usage.Poll <= 0 {
		cfg.Usage.Poll = Default().Usage.Poll
	}
	var ws []Window
	for _, w := range cfg.Usage.Windows {
		if w.Span > 0 {
			ws = append(ws, w)
		}
	}
	cfg.Usage.Windows = ws
	if err := checkLimits(cfg.Limits); err != nil {
		return cfg, err
	}
	if cfg.Narrator.DailyCapUSD < 0 {
		return cfg, fmt.Errorf("narrator.daily_cap_usd %v: must not be negative", cfg.Narrator.DailyCapUSD)
	}
	for i, r := range cfg.Regen {
		if len(r.Paths) == 0 || strings.TrimSpace(r.Cmd) == "" {
			return cfg, fmt.Errorf("regen[%d]: needs both paths and cmd", i)
		}
	}
	if cfg.Train.MaxAttempts < 1 {
		cfg.Train.MaxAttempts = Default().Train.MaxAttempts
	}
	if cfg.Train.Output == "" {
		cfg.Train.Output = Default().Train.Output
	}
	if !slices.Contains(Outputs, cfg.Train.Output) {
		return cfg, fmt.Errorf("train.output %q: want one of %s", cfg.Train.Output, strings.Join(Outputs, ", "))
	}
	if cfg.Train.StackBackend == "" {
		cfg.Train.StackBackend = Default().Train.StackBackend
	}
	if !slices.Contains(StackBackends, cfg.Train.StackBackend) {
		return cfg, fmt.Errorf("train.stack_backend %q: want one of %s", cfg.Train.StackBackend, strings.Join(StackBackends, ", "))
	}
	if cfg.Train.Prepublish.Parallel < 1 {
		cfg.Train.Prepublish.Parallel = 1
	}
	if cfg.Train.Prepublish.Timeout <= 0 {
		cfg.Train.Prepublish.Timeout = Default().Train.Prepublish.Timeout
	}
	if cfg.Train.StuckAfter <= 0 {
		cfg.Train.StuckAfter = Default().Train.StuckAfter
	}
	if cfg.Spawn.MaxDepth < 0 || cfg.Spawn.MaxChildren < 0 {
		return cfg, fmt.Errorf("spawn: max_depth and max_children must not be negative (0 means no cap)")
	}
	if c := cfg.Orchestrator.CompactAt; c <= 0 || c > 1 {
		return cfg, fmt.Errorf("orchestrator.compact_at %v: want a fraction above 0 and at most 1", c)
	}
	if cfg.Notices.WakeAfter <= 0 {
		cfg.Notices.WakeAfter = Default().Notices.WakeAfter
	}
	if cfg.Notices.DigestEvery <= 0 {
		cfg.Notices.DigestEvery = Default().Notices.DigestEvery
	}
	if cfg.CI.Interval <= 0 {
		cfg.CI.Interval = Default().CI.Interval
	}
	if cfg.CI.RedInterval <= 0 {
		cfg.CI.RedInterval = Default().CI.RedInterval
	}
	if cfg.CI.RedMaxInterval < cfg.CI.RedInterval {
		cfg.CI.RedMaxInterval = max(cfg.CI.RedInterval, Default().CI.RedMaxInterval)
	}
	if cfg.CI.RepairAttempts <= 0 {
		cfg.CI.RepairAttempts = Default().CI.RepairAttempts
	}
	if cfg.Sweeper.Method == "" {
		cfg.Sweeper.Method = Default().Sweeper.Method
	}
	if !slices.Contains(SweepMethods, cfg.Sweeper.Method) {
		return cfg, fmt.Errorf("sweeper.method %q: want one of %s", cfg.Sweeper.Method, strings.Join(SweepMethods, ", "))
	}
	if strings.TrimSpace(cfg.Sweeper.ReviewLabel) == "" {
		cfg.Sweeper.ReviewLabel = Default().Sweeper.ReviewLabel
	}
	return cfg, nil
}

func checkLimits(l usage.Limits) error {
	if l.WarnAt < 0 || l.WarnAt > 1 {
		return fmt.Errorf("limits.warn_at %v: want a fraction between 0 and 1", l.WarnAt)
	}
	for name, c := range map[string]usage.Cap{"five_hour": l.FiveHour, "weekly": l.Weekly} {
		if c.Tokens < 0 || c.USD < 0 {
			return fmt.Errorf("limits.%s: caps must not be negative", name)
		}
	}
	return nil
}

const Template = `# saddle per-repo config. See docs/ARCHITECTURE.md.
# base = "main"
# integration = "saddle/integration"
# concurrency = 5            # saddle concurrency N (or the TUI plan view) overrides it at runtime
# close_on_land = true
# harness = "claude"          # claude | grok
# serial = ["go.sum", "db/migrations/**"]

[test]
# cmd = "go test ./..."

# Derived files the merge train regenerates instead of merging. On a rebase
# conflict only in these paths, it takes integration's copy, runs cmd in the
# task's worktree and commits the result.
# [[regen]]
# paths = ["go.sum"]
# cmd = "go mod tidy"

[train]
# Failed lands (conflicts, red tests) before the train stops returning a
# branch to its agent and escalates it to you as needs-you.
# max_attempts = 2
# After each landing the train rebases every live agent's clean worktree onto
# integration; set this to only tell them to run saddle sync.
# no_auto_rebase = false
# PR layout: "stack" stacks dependent or same-topic tasks and puts unrelated
# ones on base; "single" is one linear stack; "per-task" stacks only when it must.
# output = "stack"
# Merge the bottom PR of a ready stack (green, mergeable, not a draft, not
# needs-human) and restack, until the stack is empty. Off by default;
# saddle automerge on|off|hold|release steers it at runtime.
# auto_merge = false
# How stacks are published: "saddle" chains each PR's base onto the one below
# it; "gh-stack" also links each stack on GitHub as native stacked PRs (needs
# the gh-stack extension and Stacked PRs on the repo, else falls back to saddle).
# stack_backend = "saddle"
# The repo's own pre-commit/lint gate. done runs it in the worktree and the
# train runs it after [test] cmd; red goes back to the agent like a red test.
# Unset, saddle detects it (pre-commit hook, pre-commit, lefthook, husky,
# make check/lint; saddle doctor shows what it found); "" turns it off.
# lint.cmd = "make check"
# The pre-publish gate: before prs pushes a layer, saddle checks out that
# layer's own tip and runs lint.cmd, [test] cmd and prepublish.cmd there, bottom
# to top. A red layer and those above it stay unpublished. Restack re-runs
# prepublish.cmd alone on each layer it re-cut, so keep it cheap.
# prepublish.cmd = "make check/spelling check/migrations"
# prepublish.parallel = 1
# prepublish.timeout = "30m"
# prepublish.off = false
# Interrupt the orchestrator once when a stack stays red or conflicting this
# long with no task fixing it.
# stuck_after = "30m"
# The test gate runs with TMPDIR and GOTMPDIR in a disk-backed scratch dir
# here (relative to the repo), swept of day-old Test*/go-build* dirs before
# each run. A gate that fails on the environment (disk quota, no space, OOM)
# is retried and never blamed on the branch.
# tmpdir = ".saddle/tmp"

[spawn]
# How deep spawn chains go below the orchestrator, and how many working
# children one task may have. 0 means no cap.
# max_depth = 3
# max_children = 8

[orchestrator]
# When the orchestrator's context passes this fraction of its window, saddle
# tells it to compact, and sends /compact itself once the orchestrator is idle
# and you aren't typing. A fraction above 0, at most 1.
# compact_at = 0.7

[notices]
# An idle agent whose action notices (failed tests, conflicts, messages)
# still haven't reached it after this long is woken anyway: saddle clears its
# input line and types the wake-up, and logs a notice_wake event.
# wake_after = "3m"
# Only questions and real decisions interrupt the orchestrator. Routine news
# (landed, merged, restacked, CI green) is rolled into one digest line at most
# this often, sent when the orchestrator is idle and you aren't typing.
# "saddle notices --all" lists everything, digested and silenced included.
# digest_every = "15m"

# Agent CLIs other than claude: the command and extra arguments.
# [adapters.codex]
# cmd = "codex"
# args = []   # e.g. sandbox/approval flags
# [adapters.grok]
# cmd = "grok"
# args = []

[triage]
# Uses TypeSafe Jev (set JEV_TOKEN; make setup asks for it) to decide which agent events reach
# the orchestrator or you, and to auto-approve routine permission prompts.
# disabled = false
# no_auto_approve = false

[claude]
# model = "opus"                 # workers
# orchestrator_model = "sonnet"  # the chat agent in the TUI
# permission_mode = "auto"

[grok]
# Used when harness = "grok". Workers run the grok CLI in tmux; the
# orchestrator chat is one headless grok turn per message, resumed across
# the session. Empty model means grok's own default. bypassPermissions lets
# workers edit without a prompt; the PreToolUse hook still denies claimed files.
# cmd = "grok"
# model = ""
# orchestrator_model = ""
# permission_mode = "bypassPermissions"

[ci]
# Polls saddle PRs' checks while saddle up runs; a failure goes to the owning
# task and the orchestrator, or to a new fix task when the owner has landed.
# interval = "10m"
# disabled = false
# The ci-red watcher holds layers stacked above a PR whose checks failed and
# spawns one repair per red head, on that layer; after repair_attempts it
# asks the orchestrator instead. It polls every red_interval, backing off to
# red_max_interval while nothing is red.
# red_interval = "2m"
# red_max_interval = "16m"
# repair_attempts = 2
# Known flaky checks ("workflow / job" or a job name): a failure of one is
# rerun once before it counts as red. Canceled, skipped and superseded runs
# never count; infra failures (runner lost, 5xx) are rerun once.
# flaky = []

[usage]
# Plan-limit bars are estimates: set cap to your plan's token budget for each
# window. A window with no cap shows only its total.
# poll = "15s"
# count_cache_reads = false
# [[usage.windows]]
# name = "5h"
# span = "5h"
# cap = 0
# [[usage.windows]]
# name = "7d"
# span = "168h"
# cap = 0

[limits]
# Plan-limit estimates for the usage strip. Each window is ok, warn (past
# warn_at of its cap) or over. A cap of 0 is unlimited; with both tokens and
# usd set, the fuller one counts. usd is the $-equivalent at API list prices.
# warn_at = 0.8
# pause_launches = false   # refuse new spawns while a window is over (spawn --force still works)
# weekly_reset = 2026-09-29T08:00:00Z  # any past weekly reset; unset means a rolling 7 days
# [limits.five_hour]
# tokens = 0
# usd = 0.0
# [limits.weekly]
# tokens = 0
# usd = 0.0
# [limits.prices.claude-opus]  # override list prices, USD per million tokens
# input = 5.0
# output = 25.0
# cache_read = 0.5
# cache_write = 6.25

[narrator]
# Narrates agent events into the chat thread with Claude Haiku. Needs
# ANTHROPIC_API_KEY in the environment and a daily cap; off otherwise. Past
# the cap, salient events still get a plain line without an API call.
# daily_cap_usd = 0.0
# model = "claude-haiku-4-5"

[sweeper]
# saddle sweep merges open saddle/ PRs that are green, mergeable, carry tests
# and are not labeled for review. Off until enabled; saddle sweep --dry-run
# only reports.
# enabled = false
# method = "squash"                # squash, merge or rebase
# review_label = "requires review" # never merged; applied to risky PRs
# dry_run = false
`
