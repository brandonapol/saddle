package app

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

func depReq(task string) DecisionReq {
	return DecisionReq{
		By: task, Class: "dependency", Subject: "github.com/aws/aws-sdk-go-v2/service/s3",
		Question: "Add the S3 SDK as a dependency?",
		Options: []DecisionOption{
			{Label: "Add it", Action: "go get github.com/aws/aws-sdk-go-v2/service/s3", Recommended: true},
			{Label: "Use net/http instead", Action: "write the client by hand"},
		},
	}
}

func tagReq() DecisionReq {
	return DecisionReq{
		By: OrchestratorID, Task: OrchestratorID, Class: "release-tag", Subject: "v1.4.0",
		Question: "Push the release tag v1.4.0?", Irreversible: true,
		Options: []DecisionOption{
			{Label: "Push it", Action: "git push origin v1.4.0", Recommended: true},
			{Label: "Wait for the owner", Action: "nothing"},
		},
	}
}

// actionNotices drains task's action notices.
func actionNotices(t *testing.T, a *App, task string) []string {
	t.Helper()
	ns, err := a.Store.TakeNotices(task, true)
	must(t, err)
	var out []string
	for _, n := range ns {
		out = append(out, n.Text)
	}
	return out
}

// #464 acceptance: an overnight run with a pending dependency approval still
// lands the work behind it once the owner pre-authorizes the class.
func TestPreauthUnblocksPendingDependencyAndItLands(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	write(t, a.Root, ".saddle/config.toml", "[stall.kinds]\nowner = \"24h\"\n")
	tk, err := a.Spawn(SpawnReq{ID: "t34", Title: "s3 upload"})
	must(t, err)
	now := time.Now().UTC()
	c, err := a.FileDecision(depReq("t34"), now)
	must(t, err)
	if !c.Open() || c.Timeout != 24*time.Hour {
		t.Fatalf("card = %+v", c)
	}
	if ns := actionNotices(t, a, OrchestratorID); len(ns) != 1 || !strings.Contains(ns[0], "S3 SDK") {
		t.Fatalf("orchestrator notices = %q", ns)
	}
	// Filing it again sends nothing: one card per blocker.
	if _, err := a.FileDecision(depReq("t34"), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ns := actionNotices(t, a, OrchestratorID); len(ns) != 0 {
		t.Fatalf("re-filed card pinged again: %q", ns)
	}

	// Overnight, nothing answers it and nothing runs it.
	if done, err := a.StallTick(now.Add(3 * time.Hour)); err != nil || len(done) != 0 {
		t.Fatalf("tick: %+v %v", done, err)
	}
	_ = actionNotices(t, a, OrchestratorID) // the morning digest, tested below

	// The owner pre-authorizes SDK packages.
	done, err := a.AllowClass(PreauthRule{Class: "dependency", Match: "github.com/aws/*/*/*", Note: "SDK packages in ADR 7"}, now.Add(4*time.Hour))
	must(t, err)
	if len(done) != 1 || done[0].By != DecidedPreauth || done[0].Answer != 1 {
		t.Fatalf("preauth closed %+v", done)
	}
	ns := actionNotices(t, a, "t34")
	if len(ns) != 1 || !strings.Contains(ns[0], "go get github.com/aws/aws-sdk-go-v2/service/s3") || !strings.Contains(ns[0], "pre-authorized") {
		t.Fatalf("t34 notices = %q", ns)
	}
	if open, _ := a.OpenDecisions(); slices.ContainsFunc(open, func(c DecisionCard) bool { return c.Kind == DecisionOwner }) {
		t.Fatalf("still open: %+v", open)
	}

	// t34 goes ahead and its work lands.
	write(t, tk.Worktree, "s3.go", "package main\n")
	commitAll(t, tk.Worktree, "s3 upload")
	must(t, a.Done("t34", "s3 upload"))
	rs, err := a.Land()
	must(t, err)
	if len(rs) != 1 || rs[0].State != store.TrainOK {
		t.Fatalf("land = %+v", rs)
	}

	// The next one of the class never waits.
	r := depReq("t34")
	r.Subject = "github.com/aws/aws-sdk-go-v2/service/sqs"
	c, err = a.FileDecision(r, now.Add(5*time.Hour))
	must(t, err)
	if c.Open() || c.By != DecidedPreauth {
		t.Fatalf("pre-authorized card = %+v", c)
	}
}

// #464 acceptance: an irreversible decision never runs its default on a
// timeout, and shows up once in the morning digest, not every cycle.
func TestIrreversibleDecisionStaysBlockedWithOneDigestEntry(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	write(t, a.Root, ".saddle/config.toml", "[decisions]\ndigest_at = \"08:00\"\n")
	night := time.Date(2026, 10, 12, 23, 0, 0, 0, time.Local)
	c, err := a.FileDecision(tagReq(), night)
	must(t, err)
	if !c.Open() || !strings.Contains(c.Render(), "never runs it without you") {
		t.Fatalf("card = %+v\n%s", c, c.Render())
	}
	// The orchestrator filed it, so it isn't pinged with its own card.
	if ns := actionNotices(t, a, OrchestratorID); len(ns) != 0 {
		t.Fatalf("notices = %q", ns)
	}
	// A reversible one beside it does run its default overnight.
	rev := depReq(OrchestratorID)
	rev.Task = OrchestratorID
	must(t, func() error { _, err := a.FileDecision(rev, night); return err }())

	var digests []string
	for at := night; at.Before(night.Add(12 * time.Hour)); at = at.Add(10 * time.Minute) {
		if _, err := a.StallTick(at); err != nil {
			t.Fatal(err)
		}
		for _, n := range actionNotices(t, a, OrchestratorID) {
			if strings.HasPrefix(n, "Decisions digest") {
				digests = append(digests, n)
			}
		}
	}
	open, err := a.OpenDecisions()
	must(t, err)
	if len(open) != 1 || open[0].ID != c.ID {
		t.Fatalf("open = %+v", open)
	}
	if len(digests) != 1 {
		t.Fatalf("%d digests, want 1: %q", len(digests), digests)
	}
	d := digests[0]
	if strings.Count(d, c.ID) != 1 || !strings.Contains(d, "can't be undone") {
		t.Fatalf("digest:\n%s", d)
	}
	if !strings.Contains(d, "Decided without you (1)") || !strings.Contains(d, "nobody answered") {
		t.Fatalf("digest lacks the auto-applied reversible card:\n%s", d)
	}

	// The owner answers it by id; the orchestrator is told to do it.
	got, err := a.AnswerDecisionID(c.ID, 1, night.Add(13*time.Hour))
	must(t, err)
	if got.By != DecidedByOwner {
		t.Fatalf("answered = %+v", got)
	}
	if ns := actionNotices(t, a, OrchestratorID); len(ns) != 1 || !strings.Contains(ns[0], "git push origin v1.4.0") {
		t.Fatalf("notices = %q", ns)
	}
}

// A [[preauth]] rule allows an irreversible class too, since the owner said
// so up front; a rule for another class or subject doesn't.
func TestPreauthRuleMatching(t *testing.T) {
	t.Parallel()
	c, err := tagReq().card()
	must(t, err)
	for _, tc := range []struct {
		r    PreauthRule
		want bool
	}{
		{PreauthRule{Class: "release-tag", Match: "v*"}, true},
		{PreauthRule{Class: "Release-Tag"}, true},
		{PreauthRule{Class: "release-tag", Match: "v2.*"}, false},
		{PreauthRule{Class: "dependency", Match: "*"}, false},
		{PreauthRule{Class: "release-tag", Option: 3}, false},
	} {
		if got := tc.r.Allows(c); got != tc.want {
			t.Errorf("%+v allows = %v, want %v", tc.r, got, tc.want)
		}
	}
	b := DecisionCard{Kind: DecisionStuckStack}
	if (PreauthRule{Class: "release-tag"}).Allows(b) {
		t.Error("a rule allowed a card with no class")
	}
}

// Rules come from the repo's config, and one added in the morning closes a
// card that waited overnight on the sentinel's next cycle.
func TestPreauthFromConfigOnTick(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	now := time.Now().UTC()
	c, err := a.FileDecision(tagReq(), now)
	must(t, err)
	write(t, a.Root, ".saddle/config.toml", "[[preauth]]\nclass = \"release-tag\"\nmatch = \"v*\"\n")
	if rs := a.PreauthRules(); len(rs) != 1 || rs[0].Match != "v*" {
		t.Fatalf("rules = %+v", rs)
	}
	done, err := a.StallTick(now.Add(time.Minute))
	must(t, err)
	if len(done) != 1 || done[0].ID != c.ID || done[0].By != DecidedPreauth {
		t.Fatalf("closed = %+v", done)
	}
	if !hasEvent(t, a, EventDecisionPreauth, c.ID) {
		t.Fatal("preauth not logged")
	}
}

func TestFileDecisionValidates(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	if _, err := a.FileDecision(DecisionReq{By: "t1"}, time.Now()); err == nil {
		t.Fatal("a decision with no question filed")
	}
	r := depReq("t1")
	r.Recommended = 5
	if _, err := a.FileDecision(r, time.Now()); err == nil {
		t.Fatal("a recommended option out of range filed")
	}
	// One option gets a "Not now" beside it.
	r = depReq("t1")
	r.Options = r.Options[:1]
	c, err := a.FileDecision(r, time.Now())
	must(t, err)
	if len(c.Options) != 2 || c.Default != 1 {
		t.Fatalf("card = %+v", c)
	}
}
