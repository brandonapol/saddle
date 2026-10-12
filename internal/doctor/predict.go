package doctor

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/config"
)

// saddle doctor --predict (#408). Cheap probes for what is likely to stop
// an unattended run, before it starts: branch protection that auto-merge
// can never satisfy, a gh token GitHub will refuse, a disk about to fill,
// a machine already loaded, Claude Code changing under a run. Each risk is
// a plain line with a fix, ranked, at most MaxRisks of them, and weighted
// by this repo's own escalations (the blocker classes of #407). The
// catalog behind it is docs/FAILURE-MODES.md.

// Severity ranks a predicted risk.
type Severity int

const (
	Low Severity = iota + 1
	Medium
	High
)

func (s Severity) String() string {
	switch s {
	case High:
		return "High"
	case Medium:
		return "Medium"
	}
	return "Low"
}

func (s Severity) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// MaxRisks caps the list: the owner reads it before walking away.
const MaxRisks = 7

// HistoryWindow is how far back the repo's escalations weigh the risks.
const HistoryWindow = 14 * 24 * time.Hour

// Catalog classes, shared with docs/FAILURE-MODES.md. The first five are the
// blocker kinds of #407; the rest refine an escalation's cause or name what
// a probe found.
const (
	ClassCIRed            = app.BlockerCIRed
	ClassHistoryCheck     = app.BlockerHistoryCheck
	ClassRepairOfRepair   = app.BlockerRepairOfRepair
	ClassStuckStack       = app.BlockerStuckStack
	ClassTrainEscalated   = app.BlockerTrainEscalated
	ClassBranchProtection = "branch_protection"
	ClassExternalCheck    = "external_check"
	ClassCredentials      = "credentials"
	ClassRateLimit        = "github_rate_limit"
	ClassDisk             = "disk"
	ClassLoad             = "load"
	ClassMemory           = "memory"
	ClassClaudeCLI        = "claude_cli"
	ClassUnknown          = "unknown"
)

// Risk is one predicted stopper.
type Risk struct {
	Severity Severity `json:"severity"`
	Class    string   `json:"class"`
	What     string   `json:"what"`
	Fix      string   `json:"fix"`
	// Seen is how many of this repo's escalations in HistoryWindow fall in Class.
	Seen int `json:"seen,omitempty"`
}

// Line is the risk as the owner reads it.
func (r Risk) Line() string {
	s := fmt.Sprintf("%s: %s. Fix: %s.", r.Severity, r.What, r.Fix)
	if r.Seen > 0 && r.Class != ClassUnknown {
		s += fmt.Sprintf(" (this repo: %d in %d days)", r.Seen, int(HistoryWindow.Hours()/24))
	}
	return s
}

// Escalation is one escalated blocker from the repo's history.
type Escalation struct {
	At    time.Time
	Kind  string // one of the app.Blocker* kinds
	Cause string // the event's text
}

// Disk is the free space where saddle writes.
type Disk struct {
	Label      string // "the worktrees", "tmp"
	Path       string
	Free       uint64 // bytes
	FreeInodes uint64
	Inodes     uint64 // 0: the filesystem doesn't report inodes
}

// PredictEnv is everything the probes read from outside the process.
// PredictSystem is the real one; tests use fakes.
type PredictEnv interface {
	Config() (config.Config, error)
	// GH runs gh in the repo and returns trimmed stdout.
	GH(args ...string) (string, error)
	// Exec runs a program in the repo and returns trimmed combined output.
	Exec(name string, args ...string) (string, error)
	// WorkflowFiles reports whether the repo has .github/workflows files.
	WorkflowFiles() bool
	// AutoMerge reports whether saddle merges ready stacks itself.
	AutoMerge() bool
	Disks() []Disk
	// Load is the 1-minute load average and the CPU count; 0 load is unknown.
	Load() (load1 float64, cpus int)
	// MemAvailable is the memory free for new work in bytes; 0 is unknown.
	MemAvailable() uint64
	Getenv(key string) string
	// ClaudeVersions is `claude --version` now and at the last saddle up.
	ClaudeVersions() (now, last string)
	// Escalations lists the repo's escalated blockers since.
	Escalations(since time.Time) ([]Escalation, error)
}

// Thresholds.
const (
	gb             = 1 << 30
	diskHigh       = 5 * gb
	diskMedium     = 15 * gb
	inodesHigh     = 100_000
	memPerAgent    = 3 * gb / 2
	rateLowPercent = 20
)

// Predict runs the probes and returns the ranked risks, at most MaxRisks.
func Predict(env PredictEnv, now time.Time) []Risk {
	cfg, err := env.Config()
	if err != nil {
		cfg = config.Default()
	}
	p := &predict{env: env, cfg: cfg}
	var rs []Risk
	rs = append(rs, p.protection()...)
	rs = append(rs, p.gh()...)
	rs = append(rs, p.rateLimit()...)
	rs = append(rs, p.disks()...)
	rs = append(rs, p.load()...)
	rs = append(rs, p.claude()...)
	es, _ := env.Escalations(now.Add(-HistoryWindow))
	return Rank(rs, es, cfg.CI.HistoryChecks)
}

type predict struct {
	env  PredictEnv
	cfg  config.Config
	auth bool // gh is logged in
}

// externalChecks match required checks run by an outside service with its
// own quota, sign-off or outage.
var externalChecks = regexp.MustCompile(`(?i)\b(?:codecov|snyk|vercel|netlify|sonar\w*|cla|license/cla|cla-assistant|coveralls|percy|chromatic)\b`)

func (p *predict) protection() []Risk {
	base := cmp.Or(p.cfg.Base, "main")
	out, err := p.env.GH("api", "repos/{owner}/{repo}/branches/"+base+"/protection")
	if err != nil {
		return nil // not protected or unreadable: saddle doctor says which
	}
	var pr struct {
		Checks *struct {
			Contexts []string `json:"contexts"`
			Checks   []struct {
				Context string `json:"context"`
			} `json:"checks"`
		} `json:"required_status_checks"`
		Reviews *struct {
			Count int `json:"required_approving_review_count"`
		} `json:"required_pull_request_reviews"`
		Signatures struct {
			Enabled bool `json:"enabled"`
		} `json:"required_signatures"`
	}
	if json.Unmarshal([]byte(out), &pr) != nil {
		return nil
	}
	var rs []Risk
	if pr.Reviews != nil && pr.Reviews.Count > 0 && p.env.AutoMerge() {
		rs = append(rs, Risk{Severity: High, Class: ClassBranchProtection,
			What: fmt.Sprintf("%s requires %d approving review(s) but auto-merge is on, and saddle can't approve its own PRs, so nothing will merge", base, pr.Reviews.Count),
			Fix:  "review saddle's PRs yourself and run `saddle automerge off`, or exempt saddle's PRs from the review rule in Settings -> Branches"})
	}
	if pr.Signatures.Enabled {
		if v, _ := p.env.Exec("git", "config", "--get", "commit.gpgsign"); strings.TrimSpace(v) != "true" {
			rs = append(rs, Risk{Severity: High, Class: ClassBranchProtection,
				What: base + " requires signed commits and commits here aren't signed",
				Fix:  "set git config commit.gpgsign true with a key agent that never prompts, or drop the rule"})
		}
	}
	var checks []string
	if pr.Checks != nil {
		checks = slices.Clone(pr.Checks.Contexts)
		for _, c := range pr.Checks.Checks {
			if !slices.Contains(checks, c.Context) {
				checks = append(checks, c.Context)
			}
		}
	}
	var hist, ext []string
	for _, c := range checks {
		switch {
		case app.IsHistoryCheck(c, p.cfg.CI.HistoryChecks):
			hist = append(hist, c)
		case externalChecks.MatchString(c):
			ext = append(ext, c)
		}
	}
	if len(hist) > 0 {
		r := Risk{Severity: Low, Class: ClassHistoryCheck,
			What: strings.Join(hist, ", ") + " is required and scans every commit, so a secret on a merged layer needs a human to rewrite history",
			Fix:  "keep the pre-land secret scan on and install gitleaks for a stronger one (docs/history-checks.md)"}
		if p.cfg.CI.NoSecretGate {
			r.Severity = High
			r.What = strings.Join(hist, ", ") + " is required and scans every commit, and the pre-land secret scan is off"
			r.Fix = "set no_secret_gate = false under [ci] in .saddle/config.toml"
		}
		rs = append(rs, r)
	}
	if len(ext) > 0 {
		rs = append(rs, Risk{Severity: Medium, Class: ClassExternalCheck,
			What: strings.Join(ext, ", ") + " is required and runs on an outside service with its own quota or sign-off",
			Fix:  "make it optional for saddle's PRs, or check its quota and sign-off before a long run"})
	}
	return rs
}

func (p *predict) gh() []Risk {
	out, err := p.env.Exec("gh", "auth", "status")
	if err != nil {
		return []Risk{{Severity: High, Class: ClassCredentials,
			What: "gh is not logged in (or its token expired), so saddle can't open, watch or merge PRs",
			Fix:  "gh auth login"}}
	}
	p.auth = true
	scopes, listed := tokenScopes(out)
	if !listed || scopes["workflow"] {
		return nil
	}
	if p.env.WorkflowFiles() {
		return []Risk{{Severity: High, Class: ClassCredentials,
			What: "the gh token lacks the workflow scope and this repo has .github/workflows, so GitHub rejects any push that touches them",
			Fix:  "gh auth refresh -s workflow"}}
	}
	return []Risk{{Severity: Low, Class: ClassCredentials,
		What: "the gh token lacks the workflow scope, so a branch that adds a workflow can't be pushed",
		Fix:  "gh auth refresh -s workflow"}}
}

func (p *predict) rateLimit() []Risk {
	if !p.auth {
		return nil
	}
	out, err := p.env.GH("api", "rate_limit")
	if err != nil {
		return nil
	}
	var rl struct {
		Resources struct {
			Core struct {
				Limit     int   `json:"limit"`
				Remaining int   `json:"remaining"`
				Reset     int64 `json:"reset"`
			} `json:"core"`
		} `json:"resources"`
	}
	if json.Unmarshal([]byte(out), &rl) != nil || rl.Resources.Core.Limit == 0 {
		return nil
	}
	c := rl.Resources.Core
	if c.Remaining*100 >= c.Limit*rateLowPercent {
		return nil
	}
	return []Risk{{Severity: Medium, Class: ClassRateLimit,
		What: fmt.Sprintf("only %d of %d GitHub API calls are left this hour, and every agent's PR watch spends them", c.Remaining, c.Limit),
		Fix:  fmt.Sprintf("wait for the reset at %s, or lower concurrency until then", time.Unix(c.Reset, 0).Format("15:04"))}}
}

func (p *predict) disks() []Risk {
	var rs []Risk
	for _, d := range p.env.Disks() {
		free := fmt.Sprintf("%.1f GB", float64(d.Free)/gb)
		fix := "free space there, run `saddle gc` for stale worktrees, or point TMPDIR at a bigger disk"
		switch {
		case d.Free < diskHigh:
			rs = append(rs, Risk{Severity: High, Class: ClassDisk,
				What: fmt.Sprintf("only %s free for %s (%s); worktrees and builds will fill it", free, d.Label, d.Path), Fix: fix})
		case d.Free < diskMedium:
			rs = append(rs, Risk{Severity: Medium, Class: ClassDisk,
				What: fmt.Sprintf("%s free for %s (%s); a few worktrees with builds can fill it", free, d.Label, d.Path), Fix: fix})
		}
		if d.Inodes > 0 && d.FreeInodes < inodesHigh {
			rs = append(rs, Risk{Severity: High, Class: ClassDisk,
				What: fmt.Sprintf("only %d inodes free for %s (%s); every worktree checks out the whole repo", d.FreeInodes, d.Label, d.Path), Fix: fix})
		}
	}
	return rs
}

func (p *predict) load() []Risk {
	load1, cpus := p.env.Load()
	n := max(p.cfg.Concurrency, 1)
	var rs []Risk
	if cpus > 0 {
		switch {
		case load1 >= float64(cpus):
			rs = append(rs, Risk{Severity: High, Class: ClassLoad,
				What: fmt.Sprintf("load is %.1f on %d CPUs before any agent starts, so test runs will time out", load1, cpus),
				Fix:  "stop the other heavy jobs, or lower concurrency with `saddle concurrency 1`"})
		case n*2 > cpus:
			rs = append(rs, Risk{Severity: Medium, Class: ClassLoad,
				What: fmt.Sprintf("concurrency %d on %d CPUs; parallel test runs will compete and time out", n, cpus),
				Fix:  fmt.Sprintf("`saddle concurrency %d`, or queue heavy runs (docs/runq.md)", max(cpus/2, 1))})
		}
	}
	if m := p.env.MemAvailable(); m > 0 && m/uint64(n) < memPerAgent {
		rs = append(rs, Risk{Severity: Medium, Class: ClassMemory,
			What: fmt.Sprintf("%.1f GB free for %d agents; the OOM killer will take some", float64(m)/gb, n),
			Fix:  fmt.Sprintf("close memory-heavy apps, or `saddle concurrency %d`", max(int(m/memPerAgent), 1))})
	}
	return rs
}

func (p *predict) claude() []Risk {
	now, last := p.env.ClaudeVersions()
	if now == "" {
		return nil // saddle doctor reports a missing claude
	}
	var rs []Risk
	if last != "" && now != last {
		rs = append(rs, Risk{Severity: Medium, Class: ClassClaudeCLI,
			What: fmt.Sprintf("Claude Code changed from %s to %s since the last saddle up; its stream-json or flags may have changed", last, now),
			Fix:  "spawn one small task and watch it end to end before walking away"})
	}
	if p.env.Getenv("DISABLE_AUTOUPDATER") == "" {
		rs = append(rs, Risk{Severity: Low, Class: ClassClaudeCLI,
			What: "Claude Code can update itself mid-run and change under running agents",
			Fix:  "export DISABLE_AUTOUPDATER=1 before saddle up"})
	}
	return rs
}

// causeClasses refine a generic escalation (CI red, a failed land) by its
// text. The first match wins.
var causeClasses = []struct {
	re    *regexp.Regexp
	class string
}{
	{regexp.MustCompile(`(?i)no space left|disk quota|quota exceeded|enospc|disk full`), ClassDisk},
	{regexp.MustCompile(`(?i)timed? ?out|deadline exceeded|signal: killed`), ClassLoad},
	{regexp.MustCompile(`(?i)out of memory|oom`), ClassMemory},
	{regexp.MustCompile(`(?i)workflow scope|refusing to allow .* workflow|bad credentials|authentication failed|gh auth login`), ClassCredentials},
	{regexp.MustCompile(`(?i)rate limit`), ClassRateLimit},
	{regexp.MustCompile(`(?i)review required|approving review|protected branch`), ClassBranchProtection},
	{externalChecks, ClassExternalCheck},
}

// Classify puts an escalation in a catalog class. A CI-red or failed-land
// escalation whose cause fits no class is ClassUnknown: what to add to the
// catalog next.
func Classify(e Escalation, historyChecks []string) string {
	switch e.Kind {
	case ClassHistoryCheck, ClassRepairOfRepair, ClassStuckStack:
		return e.Kind
	}
	if app.IsHistoryCheck(e.Cause, historyChecks) {
		return ClassHistoryCheck
	}
	for _, c := range causeClasses {
		if c.re.MatchString(e.Cause) {
			return c.class
		}
	}
	return ClassUnknown
}

// historyRisks word a class the repo keeps escalating on when no probe
// found it today.
var historyRisks = map[string]Risk{
	ClassHistoryCheck:     {What: "history-scanning checks (GitGuardian and the like) went red", Fix: "keep [ci] no_secret_gate off and list the repo's scanners in [ci] history_checks"},
	ClassRepairOfRepair:   {What: "CI repairs went red themselves", Fix: "look at the flaky or broken check behind them and list it in [ci] flaky"},
	ClassStuckStack:       {What: "stacks sat stuck with nobody fixing them", Fix: "answer stuck-stack cards sooner, or see docs/restack-recovery.md"},
	ClassDisk:             {What: "runs bounced on a full disk or quota", Fix: "run `saddle gc` and point TMPDIR at a bigger disk"},
	ClassLoad:             {What: "test runs timed out", Fix: "lower concurrency or queue heavy runs (docs/runq.md)"},
	ClassMemory:           {What: "runs died out of memory", Fix: "lower concurrency"},
	ClassCredentials:      {What: "pushes or gh calls were refused for credentials", Fix: "gh auth refresh -s repo,workflow"},
	ClassRateLimit:        {What: "GitHub rate limits stopped saddle", Fix: "lower concurrency"},
	ClassBranchProtection: {What: "branch protection refused merges", Fix: "check Settings -> Branches against saddle doctor"},
	ClassExternalCheck:    {What: "outside-service checks blocked merges", Fix: "make them optional for saddle's PRs"},
}

// Rank weights the probes' risks by the repo's escalations, adds a risk for
// each class the repo keeps hitting that no probe found, lists unclassified
// escalations as unknown, and sorts: severity, then how often seen. It
// returns at most MaxRisks.
func Rank(rs []Risk, es []Escalation, historyChecks []string) []Risk {
	seen := map[string]int{}
	var unknown []string
	for _, e := range es {
		c := Classify(e, historyChecks)
		seen[c]++
		if ex := truncate(e.Kind+": "+e.Cause, 60); c == ClassUnknown && len(unknown) < 2 && !slices.Contains(unknown, ex) {
			unknown = append(unknown, ex)
		}
	}
	found := map[string]bool{}
	for i := range rs {
		r := &rs[i]
		r.Seen = seen[r.Class]
		if r.Seen >= 2 && r.Severity < High {
			r.Severity++
		}
		found[r.Class] = true
	}
	for c, n := range seen {
		h, ok := historyRisks[c]
		if !ok || found[c] {
			continue
		}
		h.Class, h.Seen, h.Severity = c, n, Low
		if n >= 3 {
			h.Severity = Medium
		}
		rs = append(rs, h)
	}
	if n := seen[ClassUnknown]; n > 0 {
		rs = append(rs, Risk{Severity: Low, Class: ClassUnknown, Seen: n,
			What: fmt.Sprintf("%d escalation(s) in the last %d days fit no known failure mode (%s)", n, int(HistoryWindow.Hours()/24), strings.Join(unknown, "; ")),
			Fix:  "add them to docs/FAILURE-MODES.md so the next run probes for them"})
	}
	slices.SortStableFunc(rs, func(a, b Risk) int {
		return cmp.Or(cmp.Compare(b.Severity, a.Severity), cmp.Compare(b.Seen, a.Seen), strings.Compare(a.Class, b.Class))
	})
	if len(rs) > MaxRisks {
		rs = rs[:MaxRisks]
	}
	return rs
}

func truncate(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}

// WriteRisks prints the ranked risks, one line each.
func WriteRisks(w io.Writer, rs []Risk) {
	if len(rs) == 0 {
		fmt.Fprintln(w, "No likely stoppers found. (The catalog is docs/FAILURE-MODES.md.)")
		return
	}
	for _, r := range rs {
		fmt.Fprintln(w, r.Line())
	}
}
