package release

import (
	"slices"
	"strings"
	"testing"
)

func fixturePRs() []PR {
	return []PR{
		{Number: 10, Title: "feat: saddle upgrade", Labels: nil},
		{Number: 11, Title: "store: fix lost claims on restart", Labels: []string{"bug"}},
		{Number: 12, Title: "Multi-provider orchestration", Labels: []string{"enhancement", "saddle:ready"}},
		{Number: 13, Title: "docs: RELEASING.md"},
		{Number: 14, Title: "fix(hook): deny writes to claimed files"},
		{Number: 15, Title: "ci: cache go modules"},
		{Number: 16, Title: "feat!: rename the status --json fields"},
		{Number: 17, Title: "Tidy the TUI footer"},
		{Number: 18, Title: "Drop the v0 config keys", Labels: []string{"breaking"}},
	}
}

func TestGroupFixturePRs(t *testing.T) {
	got := map[string][]int{}
	var order []string
	for _, s := range Group(fixturePRs()) {
		order = append(order, s.Title)
		for _, p := range s.PRs {
			got[s.Title] = append(got[s.Title], p.Number)
		}
	}
	want := map[string][]int{
		"Breaking changes": {16, 18},
		"Features":         {10, 12},
		"Fixes":            {11, 14},
		"Documentation":    {13},
		"Build and CI":     {15},
		"Other changes":    {17},
	}
	wantOrder := []string{"Breaking changes", "Features", "Fixes", "Documentation", "Build and CI", "Other changes"}
	if strings.Join(order, "|") != strings.Join(wantOrder, "|") {
		t.Fatalf("section order = %v, want %v", order, wantOrder)
	}
	for k, v := range want {
		if !slices.Equal(got[k], v) {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

func TestRenderChangelog(t *testing.T) {
	out := Render("v0.1.0", "2026-10-10", fixturePRs())
	for _, want := range []string{
		"## v0.1.0 (2026-10-10)",
		"### Features",
		"- saddle upgrade (#10)",
		"- fix lost claims on restart (#11)",
		"- deny writes to claimed files (#14)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Render missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "store: fix") || strings.Contains(out, "fix(hook):") {
		t.Errorf("conventional prefixes should be stripped:\n%s", out)
	}
}

// Generated notes never carry AI co-author or attribution lines (#326).
func TestRenderStripsAttribution(t *testing.T) {
	prs := []PR{{Number: 1, Title: "Add a thing 🤖 Generated with [Claude Code](https://claude.com/claude-code)"}}
	out := Render("v0.1.0", "2026-10-10", prs)
	if strings.Contains(out, "Claude") || strings.Contains(out, "🤖") {
		t.Fatalf("attribution leaked:\n%s", out)
	}
	body := "Fixes the thing.\n\nCo-Authored-By: Claude <noreply@anthropic.com>\nClaude-Session: abc\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\nco-authored-by: someone@example.com\nKeep me."
	got := StripAttribution(body)
	if got != "Fixes the thing.\n\nKeep me." {
		t.Fatalf("StripAttribution = %q", got)
	}
}

func TestRenderNoPRs(t *testing.T) {
	if out := Render("v0.1.1", "2026-10-11", nil); !strings.Contains(out, "No merged pull requests") {
		t.Fatalf("Render(nil) = %q", out)
	}
}

func TestInsertAndExtractSection(t *testing.T) {
	cl := Header + "\n## v0.1.0 (2026-10-10)\n\n### Features\n\n- first (#1)\n"
	sec := "## v0.2.0 (2026-11-01)\n\n### Fixes\n\n- second (#2)\n"
	out, err := Insert(cl, sec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, Header) || strings.Index(out, "v0.2.0") > strings.Index(out, "v0.1.0") {
		t.Fatalf("Insert put the section in the wrong place:\n%s", out)
	}
	if _, err := Insert(out, sec); err == nil {
		t.Fatal("Insert accepted a duplicate version")
	}
	got, ok := Extract(out, "v0.1.0")
	if !ok || !strings.Contains(got, "- first (#1)") || strings.Contains(got, "second") || strings.Contains(got, "## v0.1.0") {
		t.Fatalf("Extract(v0.1.0) = %q, %v", got, ok)
	}
	if _, ok := Extract(out, "v9.9.9"); ok {
		t.Fatal("Extract found a missing version")
	}
	if out, err := Insert("", sec); err != nil || !strings.HasPrefix(out, Header) {
		t.Fatalf("Insert into empty = %q, %v", out, err)
	}
}
