package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeBlockerGH is GitHub's blocker issues in memory.
type fakeBlockerGH struct {
	issues   []BlockerIssue
	comments map[int][]string
	created  [][]string // labels per create
	reopened []int
	fail     error
}

func (g *fakeBlockerGH) Issues(state string) ([]BlockerIssue, error) {
	if g.fail != nil {
		return nil, g.fail
	}
	var out []BlockerIssue
	for _, is := range g.issues {
		if state == "all" || strings.EqualFold(is.State, state) {
			out = append(out, is)
		}
	}
	return out, nil
}

func (g *fakeBlockerGH) Create(title, body string, labels []string) (BlockerIssue, error) {
	is := BlockerIssue{Number: 500 + len(g.issues), Title: title, Body: body, State: "OPEN"}
	g.issues = append(g.issues, is)
	g.created = append(g.created, labels)
	return is, nil
}

func (g *fakeBlockerGH) Comment(n int, body string) error {
	if g.comments == nil {
		g.comments = map[int][]string{}
	}
	g.comments[n] = append(g.comments[n], body)
	return nil
}

func (g *fakeBlockerGH) EditBody(n int, body string) error {
	for i := range g.issues {
		if g.issues[i].Number == n {
			g.issues[i].Body = body
			return nil
		}
	}
	return fmt.Errorf("no issue %d", n)
}

func (g *fakeBlockerGH) Reopen(n int) error {
	g.reopened = append(g.reopened, n)
	for i := range g.issues {
		if g.issues[i].Number == n {
			g.issues[i].State = "OPEN"
		}
	}
	return nil
}

func blockerFiler(t *testing.T) (*BlockerFiler, *fakeBlockerGH) {
	t.Helper()
	a, _ := meterApp(t)
	g := &fakeBlockerGH{}
	return &BlockerFiler{App: a, GH: g, Now: func() time.Time { return time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC) }}, g
}

func TestBlockerSignatureNormalizes(t *testing.T) {
	t.Parallel()
	a := BlockerSignature(BlockerCIRed, "history_check: GitGuardian Security Checks at 3f9c2ab1 on PR #380 (t171)")
	b := BlockerSignature(BlockerCIRed, "history_check: GitGuardian Security Checks at 0d1e2f3a4b5c on PR #412 (t9)")
	if a != b {
		t.Errorf("SHAs, PR numbers and task IDs changed the signature:\n%s\n%s", a, b)
	}
	if want := "ci_red:history_check:gitguardian_security_checks_at_on"; a != want {
		t.Errorf("signature = %q, want %q", a, want)
	}
	if c := BlockerSignature(BlockerCIRed, "history_check: CI / test at 3f9c2ab1 on PR #380"); c == a {
		t.Errorf("a different check gave the same signature %q", c)
	}
	if d := BlockerSignature(BlockerTrainEscalated, "history_check: GitGuardian Security Checks"); d == a {
		t.Errorf("a different kind gave the same signature %q", d)
	}
	if got := BlockerSignature("land_timeout", "MCP https://example.com/run/123"); got != "land_timeout:mcp" {
		t.Errorf("URL not dropped: %q", got)
	}
	// An all-hex word is not a SHA.
	if got := BlockerSignature(BlockerTrainEscalated, "defaced fixture"); got != "train_escalated:defaced_fixture" {
		t.Errorf("word taken for a SHA: %q", got)
	}
}

// The first escalation opens an issue; a second with the same signature,
// on another task, PR and SHA, comments and bumps the count instead.
func TestFileBlockerCreatesThenComments(t *testing.T) {
	t.Parallel()
	f, g := blockerFiler(t)
	ctx := context.Background()
	b1 := Blocker{Kind: BlockerHistoryCheck, Cause: "GitGuardian Security Checks", Subject: "GitGuardian red on t1's unmerged layer",
		Task: "t1", PR: "#380", At: f.now()}
	r1, err := f.FileBlocker(ctx, b1)
	must(t, err)
	if !r1.Created || len(g.issues) != 1 {
		t.Fatalf("first escalation: %+v, issues %d", r1, len(g.issues))
	}
	is := g.issues[0]
	if is.Signature() != "history_check:gitguardian_security_checks" || is.Count() != 1 {
		t.Errorf("body markers: sig %q count %d\n%s", is.Signature(), is.Count(), is.Body)
	}
	if !strings.HasPrefix(is.Title, "History check: ") {
		t.Errorf("title = %q", is.Title)
	}
	for _, want := range []string{"**Model scope: Opus.**", "## Timeline", "## Design a fix so this never needs a human", "historyscan.go", "PR: #380"} {
		if !strings.Contains(is.Body, want) {
			t.Errorf("body lacks %q:\n%s", want, is.Body)
		}
	}
	if l := g.created[0]; len(l) != 2 || l[0] != BlockerLabel || l[1] != "class:history_check" {
		t.Errorf("labels = %v", l)
	}

	b2 := b1
	b2.Task, b2.PR, b2.Cause = "t171", "#412", "GitGuardian Security Checks"
	r2, err := f.FileBlocker(ctx, b2)
	must(t, err)
	if r2.Created || len(g.issues) != 1 {
		t.Fatalf("repeat opened a duplicate: %+v, issues %d", r2, len(g.issues))
	}
	cs := g.comments[is.Number]
	if len(cs) != 1 || !strings.Contains(cs[0], "Seen again 2026-10-12, t171, #412") {
		t.Errorf("comments = %q", cs)
	}
	if n := g.issues[0].Count(); n != 2 {
		t.Errorf("count after repeat = %d, want 2", n)
	}
}

// A blocker whose issue was closed reopens it rather than filing another.
func TestFileBlockerReopensClosed(t *testing.T) {
	t.Parallel()
	f, g := blockerFiler(t)
	b := Blocker{Kind: BlockerTrainEscalated, Cause: "tests failed", Task: "t2"}
	g.issues = []BlockerIssue{{Number: 7, State: "CLOSED", Body: BlockerBody(b, "s", 3)}}
	r, err := f.FileBlocker(context.Background(), b)
	must(t, err)
	if r.Created || len(g.reopened) != 1 || g.issues[0].Count() != 4 {
		t.Errorf("result %+v, reopened %v, count %d", r, g.reopened, g.issues[0].Count())
	}
}

func TestBlockerBodyIncludesHandFix(t *testing.T) {
	t.Parallel()
	f, g := blockerFiler(t)
	b := Blocker{Kind: BlockerStuckStack, Cause: "ci-red", Task: "t3",
		HandFix: "sqlite3 .saddle/state.db \"update train set state='queued' where task='t3'\""}
	_, err := f.FileBlocker(context.Background(), b)
	must(t, err)
	body := g.issues[0].Body
	if !strings.Contains(body, "## Hand fix\n```\n"+b.HandFix+"\n```") {
		t.Errorf("hand fix missing from body:\n%s", body)
	}
	if strings.Contains(BlockerBody(Blocker{Kind: BlockerStuckStack}, "s", 1), "```") {
		t.Error("a body with no hand fix shows a code block")
	}
}

// The narrator words the summary; when it fails the template stands in and
// the issue is still filed.
func TestFileBlockerDraftFallsBackToTemplate(t *testing.T) {
	t.Parallel()
	f, g := blockerFiler(t)
	f.Draft = func(context.Context, Blocker) (string, error) { return "Haiku says CI stayed red.", nil }
	_, err := f.FileBlocker(context.Background(), Blocker{Kind: BlockerCIRed, Cause: "lint", Task: "t1"})
	must(t, err)
	if !strings.Contains(g.issues[0].Body, "## Summary\nHaiku says CI stayed red.") {
		t.Errorf("drafted summary missing:\n%s", g.issues[0].Body)
	}
	f.Draft = func(context.Context, Blocker) (string, error) { return "", errors.New("cap reached") }
	_, err = f.FileBlocker(context.Background(), Blocker{Kind: BlockerCIRed, Cause: "test", Task: "t2"})
	must(t, err)
	if len(g.issues) != 2 || !strings.Contains(g.issues[1].Body, "Saddle escalated a ci red blocker on t2") {
		t.Errorf("template fallback not filed: %d issues\n%s", len(g.issues), g.issues[len(g.issues)-1].Body)
	}
}

func TestFileBlockerOptOut(t *testing.T) {
	t.Parallel()
	f, g := blockerFiler(t)
	f.App.Cfg.Issues.AutoFile = false
	f.Enabled = f.App.blockerFilingOn
	r, err := f.FileBlocker(context.Background(), Blocker{Kind: BlockerCIRed, Cause: "x"})
	must(t, err)
	if !r.Skipped || len(g.issues) != 0 {
		t.Errorf("auto_file = false still filed: %+v", r)
	}
}

// The sweep turns escalation events into issues: it skips history from
// before its first run, dedupes repeats, attaches a hand fix recorded
// before the blocker, and comments one recorded after.
func TestFileBlockersSweepsEscalationEvents(t *testing.T) {
	t.Parallel()
	f, g := blockerFiler(t)
	a := f.App
	ctx := context.Background()
	a.Store.Event("t0", eventTrainEscalated, "old; escalated after 2 failed attempts")
	res, err := f.FileBlockers(ctx)
	must(t, err)
	if len(res) != 0 || len(g.issues) != 0 {
		t.Fatalf("first sweep filed history: %+v", res)
	}

	must(t, a.SetCIRed(CIRedState{Red: []CIRedLayer{
		{Task: "t1", PR: "#380", Head: "3f9c2ab1", Checks: []string{"GitGuardian Security Checks"}},
		{Task: "t2", PR: "#381", Head: "aa11bb22", Checks: []string{"GitGuardian Security Checks"}},
	}}))
	a.Store.Event("t1", EventHandFix, "rewrote t1's layer by hand")
	a.Store.Event("t1", eventHistoryCard, "t1's layer isn't an unmerged saddle layer")
	a.Store.Event("t2", eventHistoryCard, "t2's layer isn't an unmerged saddle layer")
	a.Store.Event("t2", "spawn", "unrelated")
	res, err = f.FileBlockers(ctx)
	must(t, err)
	if len(res) != 2 || len(g.issues) != 1 || !res[0].Created || res[1].Created {
		t.Fatalf("sweep: %+v, issues %d", res, len(g.issues))
	}
	if !strings.Contains(g.issues[0].Body, "rewrote t1's layer by hand") {
		t.Errorf("hand fix recorded before the blocker missing:\n%s", g.issues[0].Body)
	}
	if !strings.Contains(g.issues[0].Body, "PR: #380") {
		t.Errorf("PR link missing:\n%s", g.issues[0].Body)
	}

	a.Store.Event("t2", EventHandFix, "acked the sentinel")
	_, err = f.FileBlockers(ctx)
	must(t, err)
	cs := g.comments[g.issues[0].Number]
	if len(cs) != 2 || !strings.Contains(cs[1], "acked the sentinel") {
		t.Errorf("later hand fix not commented: %q", cs)
	}
	if res, _ := f.FileBlockers(ctx); len(res) != 0 {
		t.Errorf("events filed twice: %+v", res)
	}
}

// A gh failure leaves the event for the next sweep.
func TestFileBlockersRetriesAfterGHFailure(t *testing.T) {
	t.Parallel()
	f, g := blockerFiler(t)
	a := f.App
	ctx := context.Background()
	_, err := f.FileBlockers(ctx)
	must(t, err)
	a.Store.Event("t1", eventTrainEscalated, "tests failed; escalated after 2 failed attempts")
	g.fail = errors.New("gh: offline")
	if _, err := f.FileBlockers(ctx); err == nil {
		t.Fatal("gh failure not reported")
	}
	g.fail = nil
	res, err := f.FileBlockers(ctx)
	must(t, err)
	if len(res) != 1 || g.issues[0].Signature() != "train_escalated:tests_failed" {
		t.Fatalf("retry: %+v %+v", res, g.issues)
	}
}

func TestOpenBlockersByCount(t *testing.T) {
	t.Parallel()
	body := func(sig string, n int) string {
		return fmt.Sprintf("x\n<!-- %s: %s -->\n<!-- %s: %d -->\n", sigMarker, sig, countMarker, n)
	}
	g := &fakeBlockerGH{issues: []BlockerIssue{
		{Number: 1, State: "OPEN", Body: body("a", 2)},
		{Number: 2, State: "OPEN", Body: body("b", 9)},
		{Number: 3, State: "CLOSED", Body: body("c", 50)},
		{Number: 4, State: "OPEN", Body: "no markers"},
	}}
	is, err := OpenBlockers(g)
	must(t, err)
	var got []int
	for _, i := range is {
		got = append(got, i.Number)
	}
	if fmt.Sprint(got) != "[2 1 4]" {
		t.Errorf("order = %v", got)
	}
}

// A stall_routed event comments on the issue already filed for the task's
// signature and never opens another.
func TestFileBlockersCommentsStallRouted(t *testing.T) {
	t.Parallel()
	f, g := blockerFiler(t)
	a := f.App
	ctx := context.Background()
	_, err := f.FileBlockers(ctx)
	must(t, err)
	a.Store.Event("t1", eventStackStuck, "t1,t2|"+StuckCIRed)
	res, err := f.FileBlockers(ctx)
	must(t, err)
	if len(res) != 1 || !res[0].Created {
		t.Fatalf("stuck stack not filed: %+v", res)
	}
	a.Store.Event("t1", eventStallRouted, "https://github.com/o/r/pull/9 held=t2 after=30m")
	res, err = f.FileBlockers(ctx)
	must(t, err)
	if len(res) != 0 || len(g.issues) != 1 {
		t.Fatalf("stall_routed opened an issue: %+v, issues %d", res, len(g.issues))
	}
	cs := g.comments[g.issues[0].Number]
	if len(cs) != 1 || !strings.Contains(cs[0], "Routed around t1 after 30m; now a standalone PR https://github.com/o/r/pull/9") || !strings.Contains(cs[0], "t2") {
		t.Errorf("route-around comment: %q", cs)
	}
	// Without a filed blocker for the task there is nothing to comment on.
	a.Store.Event("t9", eventStallRouted, "https://github.com/o/r/pull/10 held= after=1h")
	_, err = f.FileBlockers(ctx)
	must(t, err)
	if len(g.issues) != 1 || len(g.comments[g.issues[0].Number]) != 1 {
		t.Errorf("unrelated route-around touched issues: %+v", g.comments)
	}
}
