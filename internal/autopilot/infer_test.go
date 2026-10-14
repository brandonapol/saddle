package autopilot

import (
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// repoFS is a small saddle-shaped checkout.
func repoFS() fstest.MapFS {
	f := fstest.MapFS{}
	for _, p := range []string{
		"internal/autopilot/driver.go", "internal/autopilot/issue.go",
		"internal/claims/claims.go", "internal/app/app.go", "cmd/saddle/main.go",
		"docs/ARCHITECTURE.md", "docs/runq.md", "README.md", "Makefile", "go.mod", "go.sum",
		".github/ISSUE_TEMPLATE/feature.yaml", ".saddle/state.db",
	} {
		f[p] = &fstest.MapFile{Data: []byte("x")}
	}
	return f
}

func TestInferClaimsFromIssueBody(t *testing.T) {
	cases := []struct {
		name, body string
		want       []string
	}{
		{"nothing named", "Make the TUI nicer. Update the docs and/or the tests.", nil},
		{"go file claims its package",
			"The bug is in internal/autopilot/driver.go:212, where Tick skips.", []string{"internal/autopilot/**"}},
		{"package dir and doc file",
			"Touch `internal/claims` and docs/runq.md. See https://github.com/x/y/blob/main/internal/app/app.go.",
			[]string{"internal/claims/**", "docs/runq.md"}},
		{"import path maps to the repo",
			"Callers of github.com/brandonapol/saddle/internal/claims.", []string{"internal/claims/**"}},
		{"new file under an existing dir", "Add docs/tickets.md and internal/newpkg/x.go.",
			[]string{"docs/tickets.md", "internal/newpkg/**"}},
		{"glob kept, covered paths pruned", "Scope: internal/autopilot/** (driver.go lives in internal/autopilot/driver.go).",
			[]string{"internal/autopilot/**"}},
		{"root files only when named as files", "Update README.md and the Makefile; bump go.mod.", []string{"README.md"}},
		{"dotdirs", "Edit .github/ISSUE_TEMPLATE/feature.yaml.", []string{".github/ISSUE_TEMPLATE/feature.yaml"}},
		{"saddle state ignored", "Back up .saddle/state.db first.", nil},
		{"prose slashes ignored", "Read/write the saddle/integration branch, refs/saddle/wip/t1.", nil},
	}
	repo := repoFS()
	for _, c := range cases {
		if got := InferClaims(c.body, repo); !slices.Equal(got, c.want) {
			t.Errorf("%s: InferClaims(%q) = %q, want %q", c.name, c.body, got, c.want)
		}
	}
}

func TestInferClaimsWithoutARepo(t *testing.T) {
	got := InferClaims("Fix internal/autopilot/driver.go, then `internal/claims`, docs/new.md and and/or prose.", nil)
	want := []string{"internal/autopilot/**", "internal/claims/**", "docs/new.md"}
	if !slices.Equal(got, want) {
		t.Fatalf("InferClaims = %q, want %q", got, want)
	}
}

// issue506Body is the GitHub #506 issue body. The Claims line names the
// paths; "Builds on #481." and the prose above it do not.
const issue506Body = `**Difficulty: standard** (mirrors the existing grok bridge).

## Problem
The TUI orchestrator needs a headless chat process. Claude speaks stream-json; grok uses saddle's grok-bridge (internal/agent/grok_bridge.go), which runs one headless grok turn per user message. Codex has no equivalent, so OrchestratorAdapters is only claude and grok, and orchestrator failover (#481) cannot move to codex. docs/adapter-parity.md lists codex as 'n/a: no headless session'.

## Design
- Add a codex bridge modeled on the grok one: one codex exec turn per user message, resuming the same session (codex exec resume) so context carries across turns, output translated into the TUI's chat stream.
- Add Launch.headlessCodex, include codex in OrchestratorAdapters, and let orchestrator failover (internal/app/orchfailover.go) pick it.
- The orchestrator must still get saddle's MCP tools in the codex session; verify how codex loads MCP servers and wire it, or document the limit.
- Compaction: a codex equivalent for a headless session, or a fresh turn with a state summary.
- Update docs/adapter-parity.md.

## Done when
Tests (with a fake codex binary, like the grok bridge tests) show: a multi-turn chat keeps context via resume; failover lands the orchestrator on codex when claude and grok are both out; the parity doc row is updated.

Claims: internal/agent/**, internal/app/orchfailover.go, docs/adapter-parity.md. Builds on #481.
`

// The #506 body infers the Claims line and nothing else: no issue ref, no
// bare words, no trailing period, and the Go file stays a file.
func TestIssue506BodyClaims(t *testing.T) {
	repo := repoFS()
	for _, p := range []string{
		"internal/agent/grok_bridge.go",
		"internal/app/orchfailover.go",
		"docs/adapter-parity.md",
	} {
		repo[p] = &fstest.MapFile{Data: []byte("x")}
	}
	want := []string{"internal/agent/**", "internal/app/orchfailover.go", "docs/adapter-parity.md"}
	if got := InferClaims(issue506Body, repo); !slices.Equal(got, want) {
		t.Errorf("InferClaims = %q, want %q", got, want)
	}
	got, inferred := TicketClaims(issue506Body, repo)
	if inferred || !slices.Equal(got, want) {
		t.Errorf("TicketClaims = %q inferred=%v, want declared %q", got, inferred, want)
	}
}

func TestExplicitClaimLineDropsJunk(t *testing.T) {
	repo := repoFS()
	cases := []struct {
		name, body string
		want       []string
	}{
		{"literal files, prose and junk dropped",
			"Rewrite internal/claims.\n\nClaims: internal/app/app.go, missing/nope.go. Builds on #12.",
			[]string{"internal/app/app.go", "missing/nope.go"}},
		{"url bare word and issue ref",
			"Claims: https://github.com/x/y/blob/main/internal/app/app.go docs/runq.md, Builds, #9.",
			[]string{"docs/runq.md"}},
		{"trailing punctuation",
			"Claims: docs/runq.md.; internal/app/app.go:",
			[]string{"docs/runq.md", "internal/app/app.go"}},
	}
	for _, c := range cases {
		if got := InferClaims(c.body, repo); !slices.Equal(got, c.want) {
			t.Errorf("%s: InferClaims = %q, want %q", c.name, got, c.want)
		}
		got, inferred := TicketClaims(c.body, repo)
		if inferred || !slices.Equal(got, c.want) {
			t.Errorf("%s: TicketClaims = %q inferred=%v, want %q", c.name, got, inferred, c.want)
		}
	}
}

func TestDeclaredClaimsBeatInference(t *testing.T) {
	body := "Claims: internal/claims/**\n\nAlso mentions internal/app/app.go."
	got, inferred := TicketClaims(body, repoFS())
	if inferred || !slices.Equal(got, []string{"internal/claims/**"}) {
		t.Fatalf("TicketClaims = %q inferred=%v, want the declared line", got, inferred)
	}
	got, inferred = TicketClaims("Fix internal/app/app.go.", repoFS())
	if !inferred || !slices.Equal(got, []string{"internal/app/**"}) {
		t.Fatalf("TicketClaims = %q inferred=%v, want inferred internal/app/**", got, inferred)
	}
}

// Three tickets that name disjoint files, with no Claims: line, start at once.
func TestInferredDisjointTicketsSpawnTogether(t *testing.T) {
	r := newRig(t, 4,
		Issue{Number: 1, Title: "a", Body: "The bug is in internal/autopilot/driver.go."},
		Issue{Number: 2, Title: "b", Body: "Rewrite `internal/claims`."},
		Issue{Number: 3, Title: "c", Body: "Document it in docs/runq.md."},
		Issue{Number: 4, Title: "d", Body: "Same package as #1: internal/autopilot/issue.go."})
	r.d.Repo = repoFS()
	r.on(Options{})
	rep := r.tick()
	if got := r.env.spawned(); !slices.Equal(got, []int{1, 2, 3}) {
		t.Fatalf("spawned %v, want #1-#3 together and #4 queued behind #1", got)
	}
	if !strings.Contains(rep.Skipped[4], "internal/autopilot/**") {
		t.Errorf("why #4 waits = %q", rep.Skipped[4])
	}
	if c := r.env.spawns[1].Claims; !slices.Equal(c, []string{"internal/claims/**"}) {
		t.Errorf("#2 spawned with claims %q", c)
	}
	if !r.env.hasEvent(EventSpawn + ": t1 for #1 \"a\" (opus, inferred claims internal/autopilot/**)") {
		t.Errorf("spawn event doesn't say the claims were inferred: %q", r.env.events)
	}
}
