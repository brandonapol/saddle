package automerge

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestClassifyDocsOnlyIsLow(t *testing.T) {
	r := Classify([]ChangedFile{
		{Path: "README.md", Added: 300, Deleted: 200},
		{Path: "docs/ARCHITECTURE.md", Added: 400},
	}, DefaultRiskConfig())
	if r.Tier != TierLow {
		t.Fatalf("tier = %v, want low; reasons %q", r.Tier, r.Reasons)
	}
}

func TestClassifyTestOnlyIsLow(t *testing.T) {
	r := Classify([]ChangedFile{
		{Path: "internal/app/land_test.go", Added: 700, Deleted: 650},
		{Path: "internal/app/testdata/golden.txt", Added: 20},
	}, DefaultRiskConfig())
	if r.Tier != TierLow {
		t.Fatalf("tier = %v, want low; reasons %q", r.Tier, r.Reasons)
	}
}

func TestClassifySmallCodeChangeIsLow(t *testing.T) {
	r := Classify([]ChangedFile{{Path: "internal/app/land.go", Added: 30, Deleted: 10}}, DefaultRiskConfig())
	if r.Tier != TierLow || len(r.Reasons) != 0 {
		t.Fatalf("got %v %q, want low with no reasons", r.Tier, r.Reasons)
	}
}

func TestClassifyMigrationIsHigh(t *testing.T) {
	r := Classify([]ChangedFile{
		{Path: "internal/app/land.go", Added: 3},
		{Path: "migrations/0042.sql", Added: 5},
	}, DefaultRiskConfig())
	if r.Tier != TierHigh {
		t.Fatalf("tier = %v, want high", r.Tier)
	}
	if !slices.ContainsFunc(r.Reasons, func(s string) bool {
		return strings.HasPrefix(s, "migrations/0042.sql matches high glob")
	}) {
		t.Fatalf("reasons %q don't name the migration", r.Reasons)
	}
}

func TestClassifyHighDefaults(t *testing.T) {
	for _, p := range []string{
		"db/migrations/0001_init.sql",
		"internal/auth/token.go",
		".github/workflows/ci.yml",
		"deploy/prod.yaml",
		"infra/main.tf",
	} {
		if r := Classify([]ChangedFile{{Path: p, Added: 1}}, DefaultRiskConfig()); r.Tier != TierHigh {
			t.Errorf("%s: tier = %v, want high", p, r.Tier)
		}
	}
}

func TestClassifyMediumDefaults(t *testing.T) {
	for _, p := range []string{"go.mod", "web/package.json", "schema/views.sql", ".saddle/config.toml"} {
		if r := Classify([]ChangedFile{{Path: p, Added: 1}}, DefaultRiskConfig()); r.Tier != TierMedium {
			t.Errorf("%s: tier = %v, want medium", p, r.Tier)
		}
	}
}

func TestClassify500LineChangeIsMedium(t *testing.T) {
	r := Classify([]ChangedFile{
		{Path: "internal/app/land.go", Added: 300},
		{Path: "internal/app/prs.go", Added: 150, Deleted: 50},
	}, DefaultRiskConfig())
	if r.Tier != TierMedium {
		t.Fatalf("tier = %v, want medium", r.Tier)
	}
	if len(r.Reasons) != 1 || !strings.Contains(r.Reasons[0], "500 changed lines") {
		t.Fatalf("reasons = %q, want the size rule", r.Reasons)
	}
}

func TestClassifyLargeDeletionIsHigh(t *testing.T) {
	r := Classify([]ChangedFile{{Path: "internal/app/land.go", Deleted: 600}}, DefaultRiskConfig())
	if r.Tier != TierHigh {
		t.Fatalf("tier = %v, want high; reasons %q", r.Tier, r.Reasons)
	}
}

func TestClassifyGlobOverride(t *testing.T) {
	cfg := DefaultRiskConfig()
	cfg.High = []string{"internal/billing/**"}
	cfg.Medium = []string{}
	files := []ChangedFile{{Path: "internal/billing/charge.go", Added: 2}}
	if r := Classify(files, cfg); r.Tier != TierHigh || r.Reasons[0] != "internal/billing/charge.go matches high glob internal/billing/**" {
		t.Fatalf("override: got %v %q", r.Tier, r.Reasons)
	}
	// The override replaces the defaults: a migration is no longer high,
	// and with Medium emptied go.mod is no longer medium.
	if r := Classify([]ChangedFile{{Path: "migrations/0042.sql", Added: 1}, {Path: "go.mod", Added: 1}}, cfg); r.Tier != TierLow {
		t.Fatalf("defaults survived the override: %v %q", r.Tier, r.Reasons)
	}
}

func TestClassifyThresholdOverride(t *testing.T) {
	cfg := DefaultRiskConfig()
	cfg.MediumLines = 50
	if r := Classify([]ChangedFile{{Path: "a.go", Added: 51}}, cfg); r.Tier != TierMedium {
		t.Fatalf("tier = %v, want medium", r.Tier)
	}
	cfg.MediumLines = -1
	if r := Classify([]ChangedFile{{Path: "a.go", Added: 5000}}, cfg); r.Tier != TierLow {
		t.Fatalf("disabled size rule still fired: %v %q", r.Tier, r.Reasons)
	}
}

// A zero RiskConfig must not classify everything low: unset fields take the
// defaults.
func TestClassifyZeroConfigUsesDefaults(t *testing.T) {
	if r := Classify([]ChangedFile{{Path: "migrations/0042.sql", Added: 1}}, RiskConfig{}); r.Tier != TierHigh {
		t.Fatalf("tier = %v, want high", r.Tier)
	}
	if r := Classify([]ChangedFile{{Path: "a.go", Added: 500}}, RiskConfig{}); r.Tier != TierMedium {
		t.Fatalf("tier = %v, want medium", r.Tier)
	}
}

func TestClassifyNormalizesPaths(t *testing.T) {
	if r := Classify([]ChangedFile{{Path: "./migrations/0042.sql", Added: 1}}, DefaultRiskConfig()); r.Tier != TierHigh {
		t.Fatalf("tier = %v, want high", r.Tier)
	}
}

func TestRaiseNeverLowers(t *testing.T) {
	tiers := []Tier{TierLow, TierMedium, TierHigh}
	for _, floor := range tiers {
		for _, proposed := range tiers {
			got := Raise(floor, proposed)
			if got < floor {
				t.Errorf("Raise(%v, %v) = %v, lowered the floor", floor, proposed, got)
			}
			if got != max(floor, proposed) {
				t.Errorf("Raise(%v, %v) = %v, want %v", floor, proposed, got, max(floor, proposed))
			}
		}
	}
}

func TestRaiseOrKeepErrorKeepsFloor(t *testing.T) {
	err := errors.New("classifier timed out")
	for _, floor := range []Tier{TierLow, TierMedium, TierHigh} {
		if got := RaiseOrKeep(floor, TierLow, err); got != floor {
			t.Errorf("error with floor %v: got %v", floor, got)
		}
		if got := RaiseOrKeep(floor, TierHigh, err); got != floor {
			t.Errorf("error with floor %v must ignore the proposal: got %v", floor, got)
		}
	}
	if got := RaiseOrKeep(TierLow, TierMedium, nil); got != TierMedium {
		t.Fatalf("no error: got %v, want medium", got)
	}
	if got := RaiseOrKeep(TierHigh, TierLow, nil); got != TierHigh {
		t.Fatalf("no error: got %v, want high", got)
	}
}

func TestTierStringParse(t *testing.T) {
	for _, tc := range []struct {
		t Tier
		s string
	}{{TierLow, "low"}, {TierMedium, "medium"}, {TierHigh, "high"}} {
		if tc.t.String() != tc.s {
			t.Errorf("%d.String() = %q, want %q", tc.t, tc.t.String(), tc.s)
		}
		got, err := ParseTier(" " + strings.ToUpper(tc.s) + " ")
		if err != nil || got != tc.t {
			t.Errorf("ParseTier(%q) = %v, %v", tc.s, got, err)
		}
	}
	if _, err := ParseTier("critical"); err == nil {
		t.Fatal("ParseTier accepted an unknown tier")
	}
	if TierLow >= TierMedium || TierMedium >= TierHigh {
		t.Fatal("tiers are out of order")
	}
	var tr Tier
	if err := tr.UnmarshalText([]byte("medium")); err != nil || tr != TierMedium {
		t.Fatalf("UnmarshalText: %v %v", tr, err)
	}
	if b, _ := TierHigh.MarshalText(); string(b) != "high" {
		t.Fatalf("MarshalText = %q", b)
	}
}

func TestRiskConfigValidate(t *testing.T) {
	if err := DefaultRiskConfig().Validate(); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	cfg := DefaultRiskConfig()
	cfg.High = append(cfg.High, "migrations/[")
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "migrations/[") {
		t.Fatalf("Validate = %v, want the malformed glob named", err)
	}
}
