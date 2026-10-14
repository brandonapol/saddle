// Package config loads saddle's TOML config: defaults, then
// ~/.config/saddle/config.toml, then <repo>/.saddle/config.toml.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

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
	// "claude" (default), "grok" or "codex". $SADDLE_HARNESS overrides it.
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
	Adapters AdapterMap `toml:"adapters"`
	// AdapterOrder is [adapters] order: the adapters new spawns rotate
	// through when one runs out of quota (#321).
	AdapterOrder []string `toml:"-"`
	// AdapterPolicy is the rest of [adapters]: how saddle picks an adapter
	// per task and what happens to a worker whose adapter runs out (#481).
	AdapterPolicy AdapterPolicy `toml:"-"`
	// Fallback is [fallback]: per adapter or model, the adapters and models
	// that take over while it is out of quota, in order (#483). Keys and
	// entries are "adapter" or "adapter:model", e.g. "codex:sol" =
	// ["claude:opus", "grok"]. A model with a chain is tracked out of quota
	// on its own: the adapter's other models stay usable.
	Fallback map[string][]string `toml:"fallback"`
	// Notify sends owner notifications (adapter rotation) beyond the TUI and
	// the orchestrator.
	Notify Notify `toml:"notify"`
	// Spawn caps how deep and wide spawn chains below the orchestrator go.
	Spawn Spawn `toml:"spawn"`
	// Orchestrator tunes the orchestrator session itself.
	Orchestrator Orchestrator `toml:"orchestrator"`
	// Notices tunes how queued notices reach idle agents.
	Notices Notices `toml:"notices"`
	// Tmp is saddle's scratch root and its sweeper (internal/scratch).
	Tmp Tmp `toml:"tmp"`
	// Issues tunes the GitHub issues saddle files for blockers (#407).
	Issues Issues `toml:"issues"`
}

// Issues is [issues]: saddle files or comments on a GitHub issue for every
// blocker it escalates (docs/blocker-issues.md).
type Issues struct {
	// AutoFile files blocker issues. On by default; false opts out.
	AutoFile bool `toml:"auto_file"`
}

// Tmp is [tmp]: where saddle keeps its scratch and when it sweeps it
// (#322). Zero values mean scratch's defaults; Load expands ~ in Dir and
// makes a relative Dir absolute against the repo.
type Tmp struct {
	// Dir is the scratch root; empty means <user cache dir>/saddle/tmp.
	Dir string `toml:"dir"`
	// MaxAge is how old an entry must be before a sweep removes it.
	MaxAge time.Duration `toml:"max_age"`
	// LowFreePct is the free-space percentage under which spawns are held
	// after a harder sweep; negative turns the pressure response off.
	LowFreePct float64 `toml:"low_free_pct"`
	// MaxBytes caps the scratch root's size as if it were a filesystem of
	// its own: free space is what its files leave. 0 means no cap. Written
	// as bytes or with a K, M, G or T suffix ("20G").
	MaxBytes ByteSize `toml:"max_bytes"`
}

// ByteSize is a size in bytes, read from TOML as an integer or a string
// with a binary K, M, G or T suffix.
type ByteSize int64

// UnmarshalTOML reads 1000, "1000", "64K" or "2G".
func (b *ByteSize) UnmarshalTOML(v any) error {
	switch x := v.(type) {
	case int64:
		*b = ByteSize(x)
		return nil
	case string:
		n, err := ParseByteSize(x)
		*b = n
		return err
	}
	return fmt.Errorf("want bytes or a size like \"20G\", got %v", v)
}

// ParseByteSize reads "1000", "64K", "64KB", "2G" and so on, in binary units.
func ParseByteSize(s string) (ByteSize, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	t = strings.TrimSuffix(strings.TrimSuffix(t, "IB"), "B")
	mult := int64(1)
	if i := strings.IndexAny(t, "KMGT"); i >= 0 && i == len(t)-1 {
		mult = int64(1) << (10 * (1 + strings.IndexByte("KMGT", t[i])))
		t = strings.TrimSpace(t[:i])
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil || n > math.MaxInt64/mult {
		return 0, fmt.Errorf("size %q: want bytes or a number with K, M, G or T", s)
	}
	return ByteSize(n * mult), nil
}

// resolveTmp expands ~ in t.Dir, makes it absolute against root and checks
// the numbers.
func resolveTmp(t *Tmp, root string) error {
	if d := t.Dir; d != "" {
		if d == "~" || strings.HasPrefix(d, "~/") {
			if h, err := os.UserHomeDir(); err == nil {
				d = filepath.Join(h, strings.TrimPrefix(d, "~"))
			}
		}
		if !filepath.IsAbs(d) {
			d = filepath.Join(root, d)
		}
		t.Dir = filepath.Clean(d)
	}
	switch {
	case t.MaxAge < 0:
		return fmt.Errorf("tmp.max_age %v: must not be negative", t.MaxAge)
	case t.LowFreePct > 100:
		return fmt.Errorf("tmp.low_free_pct %v: want a percentage up to 100, or negative for off", t.LowFreePct)
	case t.MaxBytes < 0:
		return fmt.Errorf("tmp.max_bytes %d: must not be negative", t.MaxBytes)
	}
	return nil
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
	// RemoteControl runs the orchestrator as an interactive Claude Code
	// session under Remote Control, as saddle up --rc does (#409).
	RemoteControl bool `toml:"remote_control"`
}

// Adapter is how saddle launches an agent CLI. An empty Cmd means the
// adapter's default binary.
type Adapter struct {
	Cmd  string   `toml:"cmd"`
	Args []string `toml:"args"`
	// Model is the adapter's default model; empty leaves it to the CLI.
	// Claude's and grok's come from [claude] and [grok] (#480).
	Model string `toml:"model"`
	// Cost is the adapter's relative cost; policy = "cost" prefers the
	// cheapest. Unset is 1 (#481).
	Cost float64 `toml:"cost"`
	// Budget is the tokens the adapter may use in five hours; policy =
	// "quota" prefers the adapter with the most of it left. Unset is no
	// known limit (#481).
	Budget int64 `toml:"budget"`
	// Kinds are the task kinds (spawn's kind=) this adapter is preferred
	// for, e.g. ["docs", "mechanical"] (#481).
	Kinds []string `toml:"kinds"`
	// ExcludeModels are models saddle never spawns this adapter's workers
	// on, e.g. codex's ["astra"]. Nil (unset) is the adapter's default
	// exclusions; an empty list excludes nothing (#482).
	ExcludeModels []string `toml:"exclude_models"`
	// Difficulties are the ticket difficulties (hard, standard, mechanical)
	// it suits; nil is DefaultRouting's (#505).
	Difficulties []string `toml:"difficulties"`
	// Capabilities are what it can do beyond code, matched against a
	// ticket's Needs:, e.g. ["review", "image-gen"]; nil is DefaultRouting's.
	Capabilities []string `toml:"capabilities"`
	// Models is the model per difficulty, e.g. {hard = "opus"}; an entry
	// replaces DefaultRouting's for that difficulty.
	Models map[string]string `toml:"models"`
}

// DefaultExcludeModels are the models excluded when [adapters.<name>]
// exclude_models is unset: codex's astra burns too many tokens (#482).
var DefaultExcludeModels = map[string][]string{HarnessCodex: {"astra"}}

// ExcludedModels is the models adapter's workers never run on.
func (c Config) ExcludedModels(adapter string) []string {
	if ad, ok := c.Adapters[adapter]; ok && ad.ExcludeModels != nil {
		return ad.ExcludeModels
	}
	return DefaultExcludeModels[adapter]
}

// ExcludedModel returns the exclude_models entry model matches for adapter,
// or "". An entry matches the model id itself or one of its words, so
// "astra" excludes "gpt-6-astra" too.
func (c Config) ExcludedModel(adapter, model string) string {
	for _, x := range c.ExcludedModels(adapter) {
		if usage.ModelIs(model, x) {
			return x
		}
	}
	return ""
}

// Adapter selection policies (#481).
const (
	PickOrder = "order" // the session default, then [adapters] order
	PickCost  = "cost"  // the cheapest [adapters.<name>] cost
	PickQuota = "quota" // the most of [adapters.<name>] budget left
	// PickBalanced spreads work: among adapters suited to the task, the one
	// with the most unused quota, then the fewest running workers (#505).
	PickBalanced = "balanced"
)

// Picks lists the [adapters] policy values.
func Picks() []string { return []string{PickBalanced, PickOrder, PickCost, PickQuota} }

// Failover modes for a worker whose adapter runs out of quota (#481).
const (
	FailoverHandoff = "handoff" // relaunch it on the next adapter with quota
	FailoverPark    = "park"    // wait for the reset
)

// AdapterPolicy is [adapters] policy, failover and handoff_wait (#481).
type AdapterPolicy struct {
	// Pick is how a spawn without adapter= chooses: PickBalanced (the
	// default), PickOrder, PickCost or PickQuota. A task kind or claims narrow the choice first.
	Pick string `toml:"policy"`
	// Failover is what a running worker does when its adapter runs out:
	// FailoverHandoff or FailoverPark.
	Failover string `toml:"failover"`
	// HandoffWait parks instead of handing off when the reset is at most
	// this far away.
	HandoffWait time.Duration `toml:"handoff_wait"`
}

// adapterKeys are the [adapters] keys that are not adapter tables.
var adapterKeys = []string{"order", "policy", "failover", "handoff_wait"}

// AdapterMap is the [adapters.<name>] tables. The [adapters] table also
// holds order, which Load reads into Config.AdapterOrder.
type AdapterMap map[string]Adapter

// UnmarshalTOML decodes the per-adapter tables and skips order. Tables from
// a later config file are merged over earlier ones.
func (m *AdapterMap) UnmarshalTOML(v any) error {
	tbl, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("adapters: want a table")
	}
	if *m == nil {
		*m = AdapterMap{}
	}
	for name, raw := range tbl {
		if slices.Contains(adapterKeys, name) {
			continue
		}
		t, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("adapters.%s: want a table", name)
		}
		var ad Adapter
		for k, v := range t {
			switch k {
			case "cmd":
				s, ok := v.(string)
				if !ok {
					return fmt.Errorf("adapters.%s.cmd: want a string", name)
				}
				ad.Cmd = s
			case "model":
				s, ok := v.(string)
				if !ok {
					return fmt.Errorf("adapters.%s.model: want a string", name)
				}
				ad.Model = s
			case "cost":
				switch n := v.(type) {
				case int64:
					ad.Cost = float64(n)
				case float64:
					ad.Cost = n
				default:
					return fmt.Errorf("adapters.%s.cost: want a number", name)
				}
				if ad.Cost < 0 {
					return fmt.Errorf("adapters.%s.cost: must not be negative", name)
				}
			case "budget":
				n, ok := v.(int64)
				if !ok || n < 0 {
					return fmt.Errorf("adapters.%s.budget: want a token count", name)
				}
				ad.Budget = n
			case "kinds":
				l, ok := v.([]any)
				if !ok {
					return fmt.Errorf("adapters.%s.kinds: want an array of strings", name)
				}
				for _, k := range l {
					s, ok := k.(string)
					if !ok {
						return fmt.Errorf("adapters.%s.kinds: want an array of strings", name)
					}
					ad.Kinds = append(ad.Kinds, strings.ToLower(strings.TrimSpace(s)))
				}
			case "exclude_models":
				l, ok := v.([]any)
				if !ok {
					return fmt.Errorf("adapters.%s.exclude_models: want an array of strings", name)
				}
				ad.ExcludeModels = []string{}
				for _, x := range l {
					s, ok := x.(string)
					if !ok {
						return fmt.Errorf("adapters.%s.exclude_models: want an array of strings", name)
					}
					if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
						ad.ExcludeModels = append(ad.ExcludeModels, s)
					}
				}
			case "difficulties":
				l, err := stringList(name, k, v)
				if err != nil {
					return err
				}
				ad.Difficulties = []string{}
				for _, d := range l {
					n := NormalizeDifficulty(d)
					if n == "" {
						return fmt.Errorf("adapters.%s.difficulties: %q: want %s", name, d, strings.Join(Difficulties(), ", "))
					}
					ad.Difficulties = append(ad.Difficulties, n)
				}
			case "capabilities":
				l, err := stringList(name, k, v)
				if err != nil {
					return err
				}
				ad.Capabilities = []string{}
				for _, c := range l {
					if c = NormalizeNeed(c); c != "" {
						ad.Capabilities = append(ad.Capabilities, c)
					}
				}
			case "models":
				t, ok := v.(map[string]any)
				if !ok {
					return fmt.Errorf("adapters.%s.models: want a table of difficulty = \"model\"", name)
				}
				ad.Models = map[string]string{}
				for d, mv := range t {
					n := NormalizeDifficulty(d)
					m, ok := mv.(string)
					if n == "" || !ok {
						return fmt.Errorf("adapters.%s.models.%s: want difficulty (%s) = \"model\"", name, d, strings.Join(Difficulties(), ", "))
					}
					ad.Models[n] = strings.TrimSpace(m)
				}
			case "args":
				l, ok := v.([]any)
				if !ok {
					return fmt.Errorf("adapters.%s.args: want an array of strings", name)
				}
				for _, a := range l {
					s, ok := a.(string)
					if !ok {
						return fmt.Errorf("adapters.%s.args: want an array of strings", name)
					}
					ad.Args = append(ad.Args, s)
				}
			default:
				return fmt.Errorf("adapters.%s: unknown key %q", name, k)
			}
		}
		(*m)[name] = ad
	}
	return nil
}

// stringList reads adapters.<name>.<key> as an array of strings.
func stringList(name, key string, v any) ([]string, error) {
	l, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("adapters.%s.%s: want an array of strings", name, key)
	}
	out := make([]string, 0, len(l))
	for _, x := range l {
		s, ok := x.(string)
		if !ok {
			return nil, fmt.Errorf("adapters.%s.%s: want an array of strings", name, key)
		}
		out = append(out, s)
	}
	return out, nil
}

// AdapterOpenCode is the opencode adapter's config name (#520).
const AdapterOpenCode = "opencode"

// AdapterNames lists the adapters [adapters] order may name.
func AdapterNames() []string {
	return []string{HarnessClaude, HarnessCodex, "gemini", HarnessGrok, AdapterOpenCode}
}

// ParseModelRef splits a [fallback] key or entry, "adapter" or
// "adapter:model", checking the adapter is one saddle knows.
func ParseModelRef(s string) (adapter, model string, err error) {
	adapter, model, _ = strings.Cut(strings.TrimSpace(s), ":")
	adapter, model = strings.ToLower(strings.TrimSpace(adapter)), strings.TrimSpace(model)
	if !slices.Contains(AdapterNames(), adapter) {
		return "", "", fmt.Errorf("%q: want adapter or adapter:model, adapter one of %s", s, strings.Join(AdapterNames(), ", "))
	}
	return adapter, model, nil
}

// checkFallback refuses a [fallback] key or entry that doesn't parse, and
// a chain that names its own key.
func checkFallback(m map[string][]string) error {
	for k, chain := range m {
		ka, km, err := ParseModelRef(k)
		if err != nil {
			return fmt.Errorf("fallback key %w", err)
		}
		for _, e := range chain {
			ea, em, err := ParseModelRef(e)
			if err != nil {
				return fmt.Errorf("fallback.%q entry %w", k, err)
			}
			if ea == ka && strings.EqualFold(em, km) {
				return fmt.Errorf("fallback.%q: the chain names its own key", k)
			}
		}
	}
	return nil
}

// FallbackFor finds the [fallback] chain for model on adapter: the key
// naming that model (usage.ModelIs, so "sol" matches "gpt-6-sol"), else
// the adapter's own key. It returns the key's model ("" for the adapter's
// key), the chain, and whether there is one.
func (c Config) FallbackFor(adapter, model string) (string, []string, bool) {
	var whole []string
	found := false
	keys := make([]string, 0, len(c.Fallback))
	for k := range c.Fallback {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		ka, km, err := ParseModelRef(k)
		if err != nil || ka != adapter {
			continue
		}
		if km == "" {
			whole, found = c.Fallback[k], true
			continue
		}
		if usage.ModelIs(model, km) {
			return km, c.Fallback[k], true
		}
	}
	return "", whole, found
}

// TracksModel reports whether model on adapter is tracked out of quota on
// its own: a [fallback] key names it. It returns the key's model.
func (c Config) TracksModel(adapter, model string) (string, bool) {
	km, _, ok := c.FallbackFor(adapter, model)
	return km, ok && km != ""
}

// Notify configures owner notifications beyond the TUI banner and the
// orchestrator's notice, e.g. when an adapter runs out of quota (#321).
type Notify struct {
	// Desktop pops a desktop notification (notify-send, or osascript on macOS).
	Desktop bool `toml:"desktop"`
	// Webhook is a URL that gets a JSON POST {"text": ...}.
	Webhook string `toml:"webhook"`
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
	// HistoryChecks adds checks that read every commit of a PR, not its
	// tree, to the known ones (GitGuardian, gitleaks, TruffleHog, DCO,
	// commit-message linters), as substrings of "workflow / job" labels.
	// A red one rewrites the layer instead of spawning a fix (#405).
	HistoryChecks []string `toml:"history_checks"`
	// NoSecretGate turns off the land gate's scan of each commit for
	// secret-shaped literals (#405).
	NoSecretGate bool `toml:"no_secret_gate"`
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
	// GOTMPDIR point into; empty means <user cache dir>/saddle/tmp/<repo hash>.
	// A value inside the repo is ignored (#184, #250).
	Tmpdir string `toml:"tmpdir"`
}

// Prepublish configures the pre-publish gate (#223). Before prs or publish
// pushes a layer, saddle checks out that layer's own tip in a scratch
// worktree and runs lint.cmd, [test] cmd and Cmd there, bottom to top; a red
// layer and everything above it stay unpublished. Restack runs only Cmd on
// each layer it re-cut, so keep it cheap (spelling, migration numbers).
type Prepublish struct {
	// Setup prepares dependencies in a gate checkout before its checks. It
	// runs after each checkout with uncached checks, so lockfile changes are
	// honored. The gate's clean environment and timeout apply to it too.
	Setup string `toml:"setup"`
	Cmd   string `toml:"cmd"`
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
	Cmd               string  `toml:"cmd"`
	Model             string  `toml:"model"`
	OrchestratorModel string  `toml:"orchestrator_model"`
	PermissionMode    string  `toml:"permission_mode"`
	Advisor           Advisor `toml:"advisor"`
}

// Advisor is the opt-in hierarchical orchestrator (#257): a lead model drives
// the orchestrator session, cheap subagents swarm lookups, and an expensive
// advisor model is consulted only at fixed checkpoints. Off by default; it
// never touches workers.
type Advisor struct {
	Enabled   bool   `toml:"enabled"`
	Lead      string `toml:"lead"`      // empty falls back to orchestrator_model
	Subagents string `toml:"subagents"` // model for the session's Claude Code subagents
	Advisor   string `toml:"advisor"`
	Workers   int    `toml:"workers"` // parallel subagents the brief asks for
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
	HarnessCodex  = "codex"
)

// HarnessEnv overrides harness for one process tree: saddle up <agent> sets
// it, so the orchestrator, its MCP server and every worker follow the name
// without the config file changing (#150).
const HarnessEnv = "SADDLE_HARNESS"

// Harnesses lists the valid harness names.
func Harnesses() []string { return []string{HarnessClaude, HarnessCodex, HarnessGrok} }

func Default() Config {
	return Config{
		Base:          "main",
		Integration:   "saddle/integration",
		Concurrency:   5,
		CloseOnLand:   true,
		Issues:        Issues{AutoFile: true},
		Harness:       HarnessClaude,
		AdapterOrder:  []string{HarnessClaude, HarnessGrok, HarnessCodex, AdapterOpenCode},
		AdapterPolicy: AdapterPolicy{Pick: PickBalanced, Failover: FailoverHandoff, HandoffWait: 15 * time.Minute},
		Claude: Claude{
			Cmd:               "claude",
			Model:             "opus",
			OrchestratorModel: "sonnet",
			PermissionMode:    "auto",
			Advisor:           Advisor{Lead: "sonnet", Subagents: "haiku", Advisor: "opus", Workers: 3},
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

// DefaultSession names the tmux session for a repo root: "saddle-<dir>" with
// every run of '.', ':' or whitespace (tmux target separators) collapsed to
// '-' and leading/trailing '-' trimmed.
func DefaultSession(root string) string {
	var b strings.Builder
	for _, r := range filepath.Base(root) {
		if r == '.' || r == ':' || r == filepath.Separator || unicode.IsSpace(r) || r == '-' {
			r = '-'
		}
		if r == '-' && strings.HasSuffix(b.String(), "-") {
			continue
		}
		b.WriteRune(r)
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		return "saddle"
	}
	return "saddle-" + name
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
		if slices.ContainsFunc(adapterKeys, func(k string) bool { return md.IsDefined("adapters", k) }) {
			var o struct {
				Adapters struct {
					Order []string `toml:"order"`
					AdapterPolicy
				} `toml:"adapters"`
			}
			if _, err := toml.DecodeFile(p, &o); err != nil {
				return cfg, err
			}
			if md.IsDefined("adapters", "order") {
				cfg.AdapterOrder = o.Adapters.Order
			}
			if md.IsDefined("adapters", "policy") {
				cfg.AdapterPolicy.Pick = o.Adapters.Pick
			}
			if md.IsDefined("adapters", "failover") {
				cfg.AdapterPolicy.Failover = o.Adapters.Failover
			}
			if md.IsDefined("adapters", "handoff_wait") {
				cfg.AdapterPolicy.HandoffWait = o.Adapters.HandoffWait
			}
		}
	}
	if err := resolveTmp(&cfg.Tmp, root); err != nil {
		return cfg, err
	}
	if cfg.Session == "" {
		cfg.Session = DefaultSession(root)
	}
	if cfg.Harness == "" {
		cfg.Harness = HarnessClaude
	}
	src := "harness"
	if h := os.Getenv(HarnessEnv); h != "" {
		cfg.Harness, src = h, "$"+HarnessEnv
	}
	if !slices.Contains(Harnesses(), cfg.Harness) {
		return cfg, fmt.Errorf("%s %q: want one of %s", src, cfg.Harness, strings.Join(Harnesses(), ", "))
	}
	for _, n := range cfg.AdapterOrder {
		if !slices.Contains(AdapterNames(), n) {
			return cfg, fmt.Errorf("adapters.order %q: want names from %s", n, strings.Join(AdapterNames(), ", "))
		}
	}
	if len(cfg.AdapterOrder) == 0 {
		cfg.AdapterOrder = Default().AdapterOrder
	}
	if p := cfg.AdapterPolicy.Pick; !slices.Contains(Picks(), p) {
		return cfg, fmt.Errorf("adapters.policy %q: want one of %s", p, strings.Join(Picks(), ", "))
	}
	if f := cfg.AdapterPolicy.Failover; f != FailoverHandoff && f != FailoverPark {
		return cfg, fmt.Errorf("adapters.failover %q: want handoff or park", f)
	}
	if cfg.AdapterPolicy.HandoffWait < 0 {
		return cfg, fmt.Errorf("adapters.handoff_wait %s: must not be negative", cfg.AdapterPolicy.HandoffWait)
	}
	if err := checkFallback(cfg.Fallback); err != nil {
		return cfg, err
	}
	if cfg.Claude.Advisor.Lead == "" {
		cfg.Claude.Advisor.Lead = cfg.Claude.OrchestratorModel
	}
	if cfg.Claude.Advisor.Workers < 1 {
		cfg.Claude.Advisor.Workers = Default().Claude.Advisor.Workers
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
# harness = "claude"          # claude | grok | codex (saddle up <name> overrides)
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
# prepublish.setup = "flutter pub get" # dependencies needed by checks in fresh worktrees
# prepublish.parallel = 1
# prepublish.timeout = "30m"
# prepublish.off = false
# Interrupt the orchestrator once when a stack stays red or conflicting this
# long with no task fixing it.
# stuck_after = "30m"
# The test gate runs with TMPDIR and GOTMPDIR in a disk-backed scratch dir
# here, swept of day-old Test*/go-build* dirs before each run. The default is
# outside the repo (<user cache dir>/saddle/tmp/<repo hash>); a value inside
# the repo is ignored. A gate that fails on the environment (disk quota, no
# space, OOM) is retried and never blamed on the branch.
# tmpdir = "/var/tmp/saddle-gate"

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
# Run the orchestrator as an interactive Claude Code session with Remote
# Control on, so you can steer it from the Claude app (saddle up --rc).
# remote_control = false

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

# The decision queue (#464): owner-gated actions wait as decision cards while
# other work runs. A reversible card runs its recommended option after
# [stall.kinds] owner (default 30m); an irreversible one (a tag push, a
# publish) waits for you. One digest a day, at digest_at local time, lists
# what waits and what saddle decided meanwhile.
# [decisions]
# digest_at = "08:00"
# Classes of action you allow up front, so they never wait overnight. match is
# a glob over the decision's subject; leave it out to allow the whole class.
# "saddle decisions allow <class> [match]" appends one.
# [[preauth]]
# class = "dependency"
# match = "github.com/aws/*"
# note = "SDK packages listed in ADR 7"
# [[preauth]]
# class = "release-tag"
# match = "v*"

# When an adapter hits its usage limit, new spawns rotate to the next one
# in this order that has quota and can run here (#321).
# [adapters]
# order = ["claude", "grok", "codex", "opencode"]
# How a spawn without adapter= picks (#481, #505): "balanced" (the most
# unused quota, then the fewest running workers, so idle allowances get
# used), "order" (the session default, then this order), "cost" (the lowest
# cost below) or "quota" (the most of its budget below left). Before the
# policy, the task's needs (capabilities), difficulty and kind narrow the
# choice, and a task with claims prefers adapters that run hooks (under
# balanced only when its claims share a directory with another live task's).
# policy = "balanced"
# A worker whose adapter runs out mid-task is relaunched on the next adapter
# with quota, in the same worktree ("handoff"), or waits for the reset
# ("park"). It parks anyway when the reset is at most handoff_wait away.
# failover = "handoff"
# handoff_wait = "15m"
# Agent CLIs other than claude: the command, extra arguments and default model.
# [adapters.codex]
# cmd = "codex"
# args = []   # e.g. sandbox/approval flags; they replace saddle's own
# model = ""  # e.g. "gpt-5-codex"; empty is codex's default
# cost = 1        # relative cost, for policy = "cost"
# budget = 0      # tokens per five hours, for policy = "quota"; 0 is unknown
# kinds = []      # task kinds it is preferred for, e.g. ["docs", "mechanical"]
# Capability table (#505). Unset keys take saddle's defaults: claude suits
# every difficulty, the others standard and mechanical.
# difficulties = ["standard", "mechanical"]   # ticket difficulties it suits
# capabilities = ["review"]   # matched against a ticket's Needs:, e.g.
#                             # image-gen, web-research, long-context, plugin-host
# models = { standard = "sol", mechanical = "luna" }   # model per difficulty
# Models saddle never spawns workers on; a spawn naming one is refused.
# Unset excludes astra, which burns too many tokens; [] excludes nothing.
# exclude_models = ["astra"]
# [adapters.grok]
# cmd = "grok"
# args = []
# [adapters.opencode]
# cmd = "opencode"
# model = ""  # provider/model, e.g. "opencode/big-pickle"; empty is opencode's default

# Fallback chains (#483): who takes over while an adapter, or one of its
# models, is out of quota. A model with a chain is marked out on its own, so
# the adapter's other models stay usable; new spawns and running workers on
# it move to the first entry that has quota, can run here and isn't in
# exclude_models. Without a chain, [adapters] policy picks.
# [fallback]
# "codex:sol" = ["claude:opus", "grok"]
# "grok" = ["claude"]

# Owner notifications on adapter rotation, beyond the TUI banner and the
# orchestrator's notice.
# [notify]
# desktop = false   # notify-send, or osascript on macOS
# webhook = ""      # gets a JSON POST {"text": "..."}

[triage]
# Uses TypeSafe Jev (set JEV_TOKEN; make setup asks for it) to decide which agent events reach
# the orchestrator or you, and to auto-approve routine permission prompts.
# disabled = false
# no_auto_approve = false

[claude]
# model = "opus"                 # workers
# orchestrator_model = "sonnet"  # the chat agent in the TUI
# permission_mode = "auto"

[claude.advisor]
# Opt-in hierarchical orchestrator (#257). When enabled, the TUI orchestrator
# runs the lead model at high effort, its Claude Code subagents on the
# subagents model, and passes --advisor so the advisor model is consulted only
# before a plan locks, when the same failure repeats, and before done. The
# advisor is never the synchronizer. Workers are unaffected. Launch fails if
# the installed claude rejects --advisor.
# enabled = false
# lead = "sonnet"       # effort high; empty falls back to orchestrator_model
# subagents = "haiku"   # file discovery, AST summaries, doc lookup
# advisor = "opus"
# workers = 3           # parallel subagents

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
