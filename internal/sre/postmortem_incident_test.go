package sre

import (
	"strings"
	"testing"
	"time"
)

func TestPostmortemFromIncidentVerified(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC)
	inc := &Incident{ID: "api-500s", Summary: "API 500s", Opened: t0, Investigators: []string{"t1", "t2"}}
	id, _ := inc.State("t1", "bad deploy", "errors at deploy", t0.Add(time.Minute))
	if err := inc.Verify(id, "t2", "canary clean", t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	inc.Close(t0.Add(time.Hour))
	rec := PostmortemFromIncident(inc)
	if rec.RootCause != "bad deploy" || rec.VerifiedBy != "t2" || rec.Verified != "canary clean" {
		t.Fatalf("record: %+v", rec)
	}
	if !strings.Contains(RenderPostmortem(rec, nil), "Verified by t2") {
		t.Fatal("render lost verifier")
	}
}

func TestPostmortemFromIncidentUnresolved(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC)
	inc := &Incident{ID: "x", Opened: t0, Investigators: []string{"t1", "t2"}}
	id, _ := inc.State("t1", "guess", "", t0)
	_ = inc.Verify(id, "t2", "ok", t0)
	_ = inc.Refute(id, "t2", "no", t0)
	inc.Close(t0.Add(time.Hour))
	rec := PostmortemFromIncident(inc)
	if rec.RootCause != "" || rec.VerifiedBy != "" || rec.Title != "x" {
		t.Fatalf("record: %+v", rec)
	}
}
