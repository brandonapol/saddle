package sre

import (
	"testing"
	"testing/fstest"

	"github.com/brandonapol/saddle/internal/claims"
)

var repo = fstest.MapFS{
	"internal/billing/invoice.go": {},
	"internal/http/handler.go":    {},
	"cmd/server/main.go":          {},
	"go.mod":                      {},
}

func kinds(ls []Lane) []LaneKind {
	var ks []LaneKind
	for _, l := range ls {
		ks = append(ks, l.Kind)
	}
	return ks
}

func has(ls []Lane, k LaneKind) bool {
	for _, l := range ls {
		if l.Kind == k {
			return true
		}
	}
	return false
}

func TestPlanLanesSampleIncidents(t *testing.T) {
	cases := map[string]struct {
		text      string
		want      int
		mustHave  []LaneKind
		mustLack  []LaneKind
		codePaths []string
	}{
		"stack trace after deploy": {
			text: "API 500s since deploy v2.3.1\npanic: nil pointer\n" +
				"github.com/acme/shop/internal/billing/invoice.go:142\n/app/internal/http/handler.go:88\nnot/in/repo.go:1",
			want:      3,
			mustHave:  []LaneKind{LaneCode, LaneLogs},
			codePaths: []string{"internal/billing/invoice.go", "internal/http/handler.go"},
		},
		"infrastructure": {
			text:     "Checkout latency spiked; postgres connections exhausted and pods OOMKilled",
			want:     3,
			mustHave: []LaneKind{LaneCode, LaneLogs, LaneInfra},
			mustLack: []LaneKind{LaneDeps},
		},
		"dependency": {
			text:     "Stripe webhooks failing with TLS certificate errors; upstream returns 429 rate limit",
			want:     3,
			mustHave: []LaneKind{LaneCode, LaneLogs, LaneDeps},
			mustLack: []LaneKind{LaneInfra},
		},
		"infra and dependency": {
			text:     "Database connection errors after upgrading the redis client library",
			want:     4,
			mustHave: []LaneKind{LaneCode, LaneLogs, LaneInfra, LaneDeps},
		},
		"vague": {
			text:     "something is wrong with the site",
			want:     3,
			mustHave: []LaneKind{LaneCode, LaneLogs, LaneInfra},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ls := PlanLanes(ParseIncident(tc.text), nil, nil, repo)
			if len(ls) != tc.want || len(ls) < 3 || len(ls) > 4 {
				t.Fatalf("lanes = %v, want %d", kinds(ls), tc.want)
			}
			for _, k := range tc.mustHave {
				if !has(ls, k) {
					t.Errorf("lanes %v lack %s", kinds(ls), k)
				}
			}
			for _, k := range tc.mustLack {
				if has(ls, k) {
					t.Errorf("lanes %v have %s", kinds(ls), k)
				}
			}
			if err := CheckDisjoint(ls); err != nil {
				t.Fatal(err)
			}
			seen := map[LaneKind]bool{}
			for _, l := range ls {
				if seen[l.Kind] {
					t.Fatalf("duplicate lane %s", l.Kind)
				}
				seen[l.Kind] = true
				if l.Kind != LaneCode && len(l.Claims) > 0 {
					t.Errorf("non-code lane %s claims %q; it is read-only", l.Kind, l.Claims)
				}
				if l.Kind == LaneCode && tc.codePaths != nil {
					if len(l.Claims) != len(tc.codePaths) {
						t.Fatalf("code claims = %q, want %q", l.Claims, tc.codePaths)
					}
					for i := range tc.codePaths {
						if l.Claims[i] != tc.codePaths[i] {
							t.Fatalf("code claims = %q, want %q", l.Claims, tc.codePaths)
						}
					}
				}
			}
		})
	}
}

func TestPlanLanesFailedCheckAddsLane(t *testing.T) {
	cfg := &Config{Checks: []Check{{Name: "deps-audit", Command: []string{"npm", "audit"}, Expect: "0 vulns"}}}
	res := []CheckResult{{Name: "deps-audit", Status: CheckFailed}}
	ls := PlanLanes(ParseIncident("postgres is slow"), cfg, res, repo)
	if len(ls) != 4 || !has(ls, LaneDeps) {
		t.Fatalf("lanes = %v, want a deps lane for the failed npm check", kinds(ls))
	}
	for _, l := range ls {
		if l.Kind == LaneDeps && (len(l.Checks) != 1 || l.Checks[0] != "deps-audit") {
			t.Fatalf("deps lane checks = %q", l.Checks)
		}
	}
}

func TestPlanLanesEndpointsGoToInfra(t *testing.T) {
	cfg := &Config{Endpoints: map[string]Endpoint{"api": {URL: "https://api.example.com"}}}
	ls := PlanLanes(ParseIncident("users report errors"), cfg, nil, repo)
	for _, l := range ls {
		if l.Kind == LaneInfra {
			if len(l.Endpoints) != 1 || l.Endpoints[0] != "api" {
				t.Fatalf("infra endpoints = %q", l.Endpoints)
			}
			return
		}
	}
	t.Fatalf("lanes = %v, want infra", kinds(ls))
}

func TestCheckDisjoint(t *testing.T) {
	ok := []Lane{{Kind: LaneCode, Claims: []string{"a/x.go"}}, {Kind: LaneDeps, Claims: []string{"go.mod"}}}
	if err := CheckDisjoint(ok); err != nil {
		t.Fatal(err)
	}
	bad := []Lane{{Kind: LaneCode, Claims: []string{"a"}}, {Kind: LaneDeps, Claims: []string{"a/x.go"}}}
	if err := CheckDisjoint(bad); err == nil {
		t.Fatal("overlapping claims: want error")
	}
}

func TestPlanLanesClaimsNeverOverlap(t *testing.T) {
	// The same file named twice and a file under a named directory still
	// yield claims that do not overlap each other.
	ls := PlanLanes(ParseIncident("internal/http/handler.go:1 internal/http/handler.go:9 /srv/internal/http/handler.go"), nil, nil, repo)
	for _, l := range ls {
		for i := range l.Claims {
			for j := i + 1; j < len(l.Claims); j++ {
				if claims.Overlap(l.Claims[i], l.Claims[j]) {
					t.Fatalf("lane %s claims overlap: %q", l.Kind, l.Claims)
				}
			}
		}
	}
}
