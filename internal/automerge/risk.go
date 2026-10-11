package automerge

// Risk tiers (#332). Classify computes a deterministic floor for a landed
// diff from its paths and sizes alone, with no model. A later classifier may
// propose a different tier; Raise and RaiseOrKeep apply that proposal so it
// can raise the floor but never lower it, and a classifier error keeps the
// floor (fail closed).

import (
	"fmt"
	"path"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Tier is how risky a change is. Tiers are ordered: TierLow < TierMedium <
// TierHigh, so tiers compare with < and max.
type Tier int

const (
	TierLow Tier = iota
	TierMedium
	TierHigh
)

var tierNames = [...]string{TierLow: "low", TierMedium: "medium", TierHigh: "high"}

// String returns "low", "medium" or "high".
func (t Tier) String() string {
	if t < TierLow || t > TierHigh {
		return fmt.Sprintf("Tier(%d)", int(t))
	}
	return tierNames[t]
}

// ParseTier parses "low", "medium" or "high", ignoring case and surrounding
// space.
func ParseTier(s string) (Tier, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	for t, name := range tierNames {
		if s == name {
			return Tier(t), nil
		}
	}
	return TierLow, fmt.Errorf("unknown risk tier %q (want low, medium or high)", s)
}

// MarshalText implements encoding.TextMarshaler, so a Tier reads and writes
// as its name in TOML and JSON.
func (t Tier) MarshalText() ([]byte, error) {
	if t < TierLow || t > TierHigh {
		return nil, fmt.Errorf("invalid risk tier %d", int(t))
	}
	return []byte(t.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler using ParseTier.
func (t *Tier) UnmarshalText(b []byte) error {
	v, err := ParseTier(string(b))
	if err != nil {
		return err
	}
	*t = v
	return nil
}

// Raise returns the higher of floor and proposed: a proposal can raise the
// floor but never lower it.
func Raise(floor, proposed Tier) Tier {
	return max(floor, proposed)
}

// RaiseOrKeep applies a classifier's result to the floor. If the classifier
// failed (err != nil) its proposal is ignored and the floor stands;
// otherwise it is Raise(floor, proposed).
func RaiseOrKeep(floor, proposed Tier, err error) Tier {
	if err != nil {
		return floor
	}
	return Raise(floor, proposed)
}

// ChangedFile is one file in a diff, as git diff --numstat reports it.
// Path is slash-separated and relative to the repo root.
type ChangedFile struct {
	Path    string
	Added   int
	Deleted int
}

// RiskConfig holds the rules for Classify. Globs are doublestar patterns
// matched against repo-relative paths ("**" spans directories). A nil glob
// list takes the default from DefaultRiskConfig; a non-nil empty list means
// no globs. A threshold of 0 takes the default; a negative one turns that
// rule off. So a zero RiskConfig classifies exactly like the defaults, never
// more leniently.
type RiskConfig struct {
	// High globs: a matching path makes the change high risk.
	High []string
	// Medium globs: a matching path makes the change at least medium,
	// unless the path is Exempt.
	Medium []string
	// Exempt globs mark tests and docs. Exempt paths don't count toward
	// the size thresholds and skip the Medium globs. High globs still
	// apply to them.
	Exempt []string
	// MediumLines: more than this many changed (added plus deleted) lines
	// outside Exempt paths makes the change medium.
	MediumLines int
	// HighDeletedLines: deleting more than this many lines outside Exempt
	// paths makes the change high.
	HighDeletedLines int
}

// Default thresholds.
const (
	DefaultMediumLines      = 400
	DefaultHighDeletedLines = 500
)

// DefaultRiskConfig returns the default rules. High: migrations, auth code,
// CI workflows and deploy/infra config. Medium: dependency manifests and
// lockfiles, SQL, and config files. Exempt: tests, test data and docs.
// Dependency files are medium, not high: telling a major version bump apart
// needs the diff's contents, which Classify doesn't see.
func DefaultRiskConfig() RiskConfig {
	return RiskConfig{
		High: []string{
			"**/migrations/**",
			"**/migrate/**",
			"**/auth/**",
			".github/workflows/**",
			"**/deploy/**",
			"**/infra/**",
			"**/terraform/**",
			"**/helm/**",
			"**/k8s/**",
			"**/*.tf",
			"**/Dockerfile",
			"**/docker-compose*.yml",
			"**/docker-compose*.yaml",
		},
		Medium: []string{
			"**/go.mod",
			"**/go.sum",
			"**/package.json",
			"**/package-lock.json",
			"**/yarn.lock",
			"**/pnpm-lock.yaml",
			"**/Cargo.toml",
			"**/Cargo.lock",
			"**/requirements*.txt",
			"**/pyproject.toml",
			"**/poetry.lock",
			"**/Gemfile",
			"**/Gemfile.lock",
			"**/*.sql",
			"**/*.toml",
			"**/*.yaml",
			"**/*.yml",
			"**/*.ini",
			"**/*.conf",
			"**/.env*",
			"**/Makefile",
		},
		Exempt: []string{
			"**/*_test.go",
			"**/testdata/**",
			"**/test/**",
			"**/tests/**",
			"**/__tests__/**",
			"**/*.test.*",
			"**/*.spec.*",
			"**/test_*.py",
			"**/*.md",
			"**/docs/**",
		},
		MediumLines:      DefaultMediumLines,
		HighDeletedLines: DefaultHighDeletedLines,
	}
}

// withDefaults fills unset fields from DefaultRiskConfig.
func (c RiskConfig) withDefaults() RiskConfig {
	d := DefaultRiskConfig()
	if c.High == nil {
		c.High = d.High
	}
	if c.Medium == nil {
		c.Medium = d.Medium
	}
	if c.Exempt == nil {
		c.Exempt = d.Exempt
	}
	if c.MediumLines == 0 {
		c.MediumLines = d.MediumLines
	}
	if c.HighDeletedLines == 0 {
		c.HighDeletedLines = d.HighDeletedLines
	}
	return c
}

// Result is a classification: the tier and why. Reasons lists every rule
// that fired at Tier, in file order with the size rules last, e.g.
// "migrations/0042.sql matches high glob **/migrations/**". A low Result
// has no reasons.
type Result struct {
	Tier    Tier
	Reasons []string
}

// Classify returns the deterministic risk floor for a diff. High globs make
// it high, as does deleting more than HighDeletedLines lines outside Exempt
// paths. Otherwise Medium globs on non-exempt paths, or more than
// MediumLines changed lines outside Exempt paths, make it medium. Anything
// else, such as a test-only or docs-only change, is low.
func Classify(files []ChangedFile, cfg RiskConfig) Result {
	cfg = cfg.withDefaults()
	var high, medium []string
	changed, deleted := 0, 0
	for _, f := range files {
		p := normPath(f.Path)
		if g, ok := firstMatch(cfg.High, p); ok {
			high = append(high, fmt.Sprintf("%s matches high glob %s", p, g))
		}
		if _, ok := firstMatch(cfg.Exempt, p); ok {
			continue
		}
		if g, ok := firstMatch(cfg.Medium, p); ok {
			medium = append(medium, fmt.Sprintf("%s matches medium glob %s", p, g))
		}
		changed += f.Added + f.Deleted
		deleted += f.Deleted
	}
	if cfg.HighDeletedLines > 0 && deleted > cfg.HighDeletedLines {
		high = append(high, fmt.Sprintf("deletes %d lines outside tests and docs (over %d)", deleted, cfg.HighDeletedLines))
	}
	if cfg.MediumLines > 0 && changed > cfg.MediumLines {
		medium = append(medium, fmt.Sprintf("%d changed lines outside tests and docs (over %d)", changed, cfg.MediumLines))
	}
	switch {
	case len(high) > 0:
		return Result{Tier: TierHigh, Reasons: high}
	case len(medium) > 0:
		return Result{Tier: TierMedium, Reasons: medium}
	}
	return Result{Tier: TierLow}
}

// normPath cleans a diff path to the slash-separated, repo-relative form the
// globs match ("./a//b" becomes "a/b").
func normPath(p string) string {
	return path.Clean(strings.TrimPrefix(strings.ReplaceAll(p, `\`, "/"), "/"))
}

// firstMatch returns the first glob in globs that matches p. A malformed
// glob never matches.
func firstMatch(globs []string, p string) (string, bool) {
	for _, g := range globs {
		if ok, _ := doublestar.Match(g, p); ok {
			return g, true
		}
	}
	return "", false
}

// Validate reports the first malformed glob in c. A malformed glob never
// matches in Classify, which would quietly lower a tier, so config loading
// should reject it.
func (c RiskConfig) Validate() error {
	for _, l := range []struct {
		name  string
		globs []string
	}{{"high", c.High}, {"medium", c.Medium}, {"exempt", c.Exempt}} {
		for _, g := range l.globs {
			if !doublestar.ValidatePattern(g) {
				return fmt.Errorf("risk %s glob %q is malformed", l.name, g)
			}
		}
	}
	return nil
}
