package sre

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 14, 3, 0, 0, 0, time.UTC)

func newIncident() *Incident {
	return &Incident{ID: "db-down", Summary: "API 500s", Opened: t0, Investigators: []string{"t1", "t2", "t3"}}
}

func TestGuardLockedUntilRootCauseVerified(t *testing.T) {
	inc := newIncident()
	if _, err := inc.Propose("t1", "kubectl rollout undo deploy/api", t0); !errors.Is(err, ErrLocked) {
		t.Fatalf("propose with no root cause: err = %v, want ErrLocked", err)
	}
	id, err := inc.State("t1", "bad migration 42 dropped an index", "slow query log shows seq scans", t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inc.Propose("t1", "kubectl rollout undo deploy/api", t0); !errors.Is(err, ErrLocked) {
		t.Fatalf("propose with an unverified root cause: err = %v, want ErrLocked", err)
	}
	if _, ok := inc.Unlocked(); ok {
		t.Fatal("unlocked before verification")
	}
	if err := inc.Verify(id, "t2", "EXPLAIN on prod replica confirms the missing index", t0); err != nil {
		t.Fatal(err)
	}
	h, ok := inc.Unlocked()
	if !ok || h.ID != id {
		t.Fatalf("Unlocked() = %+v, %v; want hypothesis %d", h, ok, id)
	}
	p, err := inc.Propose("t3", "kubectl rollout undo deploy/api", t0)
	if err != nil {
		t.Fatal(err)
	}
	if p.By != "t3" || p.Hypothesis != id || p.StatedBy != "t1" || p.VerifiedBy != "t2" || p.Command != "kubectl rollout undo deploy/api" {
		t.Fatalf("proposal = %+v", p)
	}
	if len(inc.Proposals) != 1 {
		t.Fatalf("proposals recorded = %d, want 1", len(inc.Proposals))
	}
}

func TestGuardVerifierMustDifferFromProposer(t *testing.T) {
	inc := newIncident()
	id, _ := inc.State("t1", "cert expired", "openssl s_client shows notAfter yesterday", t0)
	if err := inc.Verify(id, "t1", "I checked again", t0); !errors.Is(err, ErrSameAgent) {
		t.Fatalf("self-verify: err = %v, want ErrSameAgent", err)
	}
	if _, ok := inc.Unlocked(); ok {
		t.Fatal("self-verification unlocked the guard")
	}
	if _, err := inc.Propose("t1", "renew-cert", t0); !errors.Is(err, ErrLocked) {
		t.Fatalf("propose after self-verify: err = %v, want ErrLocked", err)
	}
}

func TestGuardVerifyNeedsEvidenceAndAKnownHypothesis(t *testing.T) {
	inc := newIncident()
	id, _ := inc.State("t1", "cert expired", "notAfter yesterday", t0)
	if err := inc.Verify(id, "t2", "  ", t0); err == nil {
		t.Fatal("verify without evidence passed")
	}
	if err := inc.Verify(id+1, "t2", "seen", t0); err == nil {
		t.Fatal("verify of an unknown hypothesis passed")
	}
	if err := inc.Verify(id, "", "seen", t0); err == nil {
		t.Fatal("verify by nobody passed")
	}
	if _, err := inc.State("t1", "", "x", t0); err == nil {
		t.Fatal("empty root cause accepted")
	}
}

func TestGuardRefuteRelocks(t *testing.T) {
	inc := newIncident()
	id, _ := inc.State("t1", "cert expired", "notAfter yesterday", t0)
	if err := inc.Verify(id, "t2", "confirmed", t0); err != nil {
		t.Fatal(err)
	}
	if err := inc.Refute(id, "t3", "the cert was renewed an hour ago; errors continue", t0); err != nil {
		t.Fatal(err)
	}
	if _, ok := inc.Unlocked(); ok {
		t.Fatal("refuted hypothesis still unlocks")
	}
	if err := inc.Verify(id, "t3", "x", t0); !errors.Is(err, ErrRefuted) {
		t.Fatalf("verify refuted: err = %v, want ErrRefuted", err)
	}
}

func TestGuardGuardsInvestigatorsOfAnOpenIncident(t *testing.T) {
	inc := newIncident()
	if !inc.Guarded("t2") || inc.Guarded("t9") {
		t.Fatal("Guarded wrong for listed/unlisted task")
	}
	inc.Close(t0)
	if inc.Guarded("t2") {
		t.Fatal("closed incident still guards")
	}
	var nilInc *Incident
	if nilInc.Guarded("t2") {
		t.Fatal("no incident guards")
	}
}

func TestGuardReadOnlyChecksFromConfig(t *testing.T) {
	c := &Config{Checks: []Check{
		{Name: "db-pop", Command: []string{"psql", "-c", "select count(*) from users"}, Expect: ">0"},
		{Name: "api", Surface: "endpoint:api", Expect: "200"},
	}}
	inc := newIncident()
	inc.AllowChecks(c)
	if len(inc.ReadOnly) != 1 || inc.ReadOnly[0][0] != "psql" {
		t.Fatalf("ReadOnly = %v", inc.ReadOnly)
	}
}

func TestGuardStoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	if inc, err := LoadIncident(root); err != nil || inc != nil {
		t.Fatalf("LoadIncident with none = %v, %v", inc, err)
	}
	if err := OpenIncident(root, newIncident()); err != nil {
		t.Fatal(err)
	}
	if err := OpenIncident(root, newIncident()); err == nil {
		t.Fatal("opened a second incident over an open one")
	}
	var id int
	if err := UpdateIncident(root, func(inc *Incident) error {
		var err error
		id, err = inc.State("t1", "cert expired", "notAfter yesterday", t0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// A failed update leaves the file alone.
	if err := UpdateIncident(root, func(inc *Incident) error { return inc.Verify(id, "t1", "me", t0) }); !errors.Is(err, ErrSameAgent) {
		t.Fatalf("err = %v", err)
	}
	inc, err := LoadIncident(root)
	if err != nil || inc == nil || len(inc.Hypotheses) != 1 || inc.Hypotheses[0].Verified != nil {
		t.Fatalf("LoadIncident = %+v, %v", inc, err)
	}
	if err := UpdateIncident(root, func(inc *Incident) error { inc.Close(t0); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := OpenIncident(root, &Incident{ID: "next", Investigators: []string{"t4"}}); err != nil {
		t.Fatalf("open after close: %v", err)
	}
}

func TestGuardStoreCorruptFileIsAnError(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, IncidentFile)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadIncident(root); err == nil {
		t.Fatal("corrupt incident file loaded")
	}
}
