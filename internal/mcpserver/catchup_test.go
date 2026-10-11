package mcpserver

import (
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
)

// status carries the headline, and a flagged stack makes it blocked (#403).
func TestStatusCarriesHeadline(t *testing.T) {
	a, _ := stackSetup(t)
	if err := a.SetFlag(app.StackFlag{Task: "t1", Cause: "conflicts with its base"}); err != nil {
		t.Fatal(err)
	}
	h := callStatus(t, a).Headline
	if h.Class != app.HeadlineBlocked || h.Color != app.ColorRed || !strings.Contains(h.Text, "stack is at risk at t1") {
		t.Fatalf("headline = %+v", h)
	}
}

// The catchup tool returns bullets since a given duration and refuses a
// since it can't read (#403).
func TestCatchupTool(t *testing.T) {
	a, _ := stackSetup(t)
	a.Store.Event("t1", "landed", "")
	var out CatchUpOut
	if res := callTool(t, a, app.OrchestratorID, "catchup", map[string]any{"since": "1h"}, &out); res.IsError {
		t.Fatalf("catchup failed: %+v", res.Content)
	}
	if len(out.Bullets) == 0 || len(out.Bullets) > 5 || !strings.Contains(strings.Join(out.Bullets, "\n"), "1 landed (t1") {
		t.Fatalf("bullets = %q", out.Bullets)
	}
	if time.Since(out.Since) < 59*time.Minute {
		t.Fatalf("since = %v, want an hour ago", out.Since)
	}
	if res := callTool(t, a, app.OrchestratorID, "catchup", map[string]any{"since": "last week"}, &out); !res.IsError {
		t.Fatal("a bad since was accepted")
	}
}
