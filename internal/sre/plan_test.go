package sre

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeSpawner struct {
	specs []SpawnSpec
	fail  map[LaneKind]bool
}

func (f *fakeSpawner) Spawn(s SpawnSpec) (string, error) {
	f.specs = append(f.specs, s)
	if f.fail[s.Lane] {
		return "", errors.New("claim conflict")
	}
	return "t" + string(s.Lane), nil
}

const planCfg = `
[credentials.cluster]
ref = "kube-context:prod"
[endpoints.api]
url = "https://api.example.com/healthz"
credential = "cluster"

[[check]]
name = "pods"
command = ["kubectl", "get", "pods"]
expect = "all Running"
credentials = ["cluster"]
`

func TestBuildRunsChecksAndPlans(t *testing.T) {
	cfg, err := Parse([]byte(planCfg))
	if err != nil {
		t.Fatal(err)
	}
	r := &fakeRunner{out: map[string]string{"kubectl": "web-1 CrashLoopBackOff"}, fail: map[string]bool{"kubectl": true}}
	p, err := Build(context.Background(), "500s from internal/http/handler.go:88", cfg, ".", repo, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 1 || r.calls[0][0] != "kubectl" {
		t.Fatalf("preemptive checks ran %q", r.calls)
	}
	if len(p.Checks) != 1 || p.Checks[0].Status != CheckFailed {
		t.Fatalf("checks = %+v", p.Checks)
	}
	if n := len(p.Lanes); n < 3 || n > 4 {
		t.Fatalf("lanes = %v", kinds(p.Lanes))
	}
	if _, err := Build(context.Background(), "  \n", cfg, ".", repo, r); err == nil {
		t.Fatal("empty incident: want error")
	}
}

func TestLaunchSpawnsReadOnlyInvestigators(t *testing.T) {
	cfg, err := Parse([]byte(planCfg))
	if err != nil {
		t.Fatal(err)
	}
	r := &fakeRunner{out: map[string]string{"kubectl": "web-1 CrashLoopBackOff"}, fail: map[string]bool{"kubectl": true}}
	p, err := Build(context.Background(), "500s from internal/http/handler.go:88 after redis library upgrade", cfg, ".", repo, r)
	if err != nil {
		t.Fatal(err)
	}
	sp := &fakeSpawner{fail: map[LaneKind]bool{LaneDeps: true}}
	got, err := Launch(p, sp)
	if err == nil || !strings.Contains(err.Error(), "deps") {
		t.Fatalf("err = %v, want the deps lane failure", err)
	}
	if len(sp.specs) != len(p.Lanes) {
		t.Fatalf("spawned %d of %d lanes; one failure must not stop the rest", len(sp.specs), len(p.Lanes))
	}
	if len(got) != len(p.Lanes)-1 {
		t.Fatalf("launched = %+v", got)
	}
	for _, s := range sp.specs {
		if !strings.Contains(s.Prompt, "read-only") || !strings.Contains(s.Prompt, "internal/http/handler.go:88") {
			t.Errorf("%s prompt lacks the read-only rule or the incident:\n%s", s.Lane, s.Prompt)
		}
		if !strings.Contains(s.Prompt, "CrashLoopBackOff") {
			t.Errorf("%s prompt lacks the preemptive check results", s.Lane)
		}
		if strings.HasPrefix(s.Title, "saddle:") || !strings.HasPrefix(s.Title, "sre "+string(s.Lane)) {
			t.Errorf("title = %q", s.Title)
		}
		switch s.Lane {
		case LaneCode:
			if len(s.Claims) != 1 || s.Claims[0] != "internal/http/handler.go" {
				t.Errorf("code claims = %q", s.Claims)
			}
		default:
			if len(s.Claims) != 0 {
				t.Errorf("%s claims = %q, want none", s.Lane, s.Claims)
			}
		}
		if s.Lane == LaneInfra && !strings.Contains(s.Prompt, "kube-context:prod") {
			t.Errorf("infra prompt lacks the credential ref:\n%s", s.Prompt)
		}
	}
}

func TestLaunchRefusesOverlappingClaims(t *testing.T) {
	p := &Plan{Incident: ParseIncident("x"), Lanes: []Lane{
		{Kind: LaneCode, Claims: []string{"a"}},
		{Kind: LaneDeps, Claims: []string{"a/b.go"}},
		{Kind: LaneLogs},
	}}
	sp := &fakeSpawner{}
	if _, err := Launch(p, sp); err == nil || len(sp.specs) != 0 {
		t.Fatalf("err = %v, spawned %d; want refusal before any spawn", err, len(sp.specs))
	}
}
