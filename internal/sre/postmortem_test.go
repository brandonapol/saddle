package sre

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fullIncident() Incident {
	s := time.Date(2026, 10, 14, 9, 30, 0, 0, time.UTC)
	return Incident{
		Title:         "Users table empty in prod!",
		Start:         s,
		End:           s.Add(95 * time.Minute),
		Timeline:      []TimelineEntry{{At: s.Add(10 * time.Minute), Text: "db-population check failed"}, {At: s, Text: "alert fired"}},
		Findings:      []LaneFinding{{Lane: "db", Summary: "users has 0 rows"}},
		RootCause:     "A migration truncated users.",
		VerifiedBy:    "agent-b",
		Verified:      "re-ran db-population read-only",
		FixPRs:        []string{"#601"},
		FollowUps:     []string{"Guard migrations in CI"},
		FoundByChecks: []string{"db-population", "gone-check"},
	}
}

func testCfg(t *testing.T) *Config {
	t.Helper()
	c, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRenderFull(t *testing.T) {
	out := RenderPostmortem(fullIncident(), testCfg(t))
	for _, want := range []string{
		"# Postmortem: Users table empty in prod!",
		"## Timeline", "## Root cause", "## Verification", "## Fix", "## Follow-ups",
		"## Lane findings", "## Proposed runbook additions",
		"A migration truncated users.", "agent-b", "#601", "Guard migrations in CI",
		"users has 0 rows", "1h35m",
		"db-population", "select count(*) from users", "count greater than 0",
		"`gone-check`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Timeline is sorted chronologically.
	if strings.Index(out, "alert fired") > strings.Index(out, "db-population check failed") {
		t.Errorf("timeline not sorted:\n%s", out)
	}
	if strings.Contains(out, "PROD_DATABASE_URL") {
		t.Error("credential details leaked")
	}
}

func TestRenderMissingOptional(t *testing.T) {
	out := RenderPostmortem(Incident{Title: "Odd blip", Start: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)}, nil)
	for _, want := range []string{"# Postmortem: Odd blip", "## Timeline", "## Root cause", "## Verification", "## Fix", "## Follow-ups", "Unknown", "ongoing"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, not := range []string{"## Lane findings", "## Proposed runbook additions"} {
		if strings.Contains(out, not) {
			t.Errorf("unexpected %q", not)
		}
	}
}

func TestSlugAndName(t *testing.T) {
	if got := PostmortemSlug("  Users table: EMPTY in prod!! "); got != "users-table-empty-in-prod" {
		t.Errorf("slug = %q", got)
	}
	if got := PostmortemSlug("!!!"); got != "incident" {
		t.Errorf("empty slug = %q", got)
	}
	long := PostmortemSlug(strings.Repeat("abc ", 50))
	if len(long) > 60 || strings.HasSuffix(long, "-") {
		t.Errorf("long slug = %q", long)
	}
	if got := PostmortemName(fullIncident()); got != "2026-10-14-users-table-empty-in-prod.md" {
		t.Errorf("name = %q", got)
	}
}

func TestWriteNoOverwrite(t *testing.T) {
	root := t.TempDir()
	inc := fullIncident()
	p1, err := WritePostmortem(root, inc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(p1) != "2026-10-14-users-table-empty-in-prod.md" || filepath.Dir(p1) != filepath.Join(root, PostmortemDir) {
		t.Fatalf("p1 = %s", p1)
	}
	orig, _ := os.ReadFile(p1)
	p2, err := WritePostmortem(root, inc, nil)
	if err != nil {
		t.Fatal(err)
	}
	p3, _ := WritePostmortem(root, inc, nil)
	if filepath.Base(p2) != "2026-10-14-users-table-empty-in-prod-2.md" || filepath.Base(p3) != "2026-10-14-users-table-empty-in-prod-3.md" {
		t.Fatalf("p2 = %s p3 = %s", p2, p3)
	}
	if now, _ := os.ReadFile(p1); string(now) != string(orig) {
		t.Error("existing postmortem was modified")
	}
}

func TestWriteDoesNotTouchRunbooks(t *testing.T) {
	root := t.TempDir()
	if _, err := WritePostmortem(root, fullIncident(), testCfg(t)); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(root)
	if len(ents) != 1 || ents[0].Name() != "docs" {
		t.Fatalf("unexpected files: %v", ents)
	}
}
