package planner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// fakeGH answers issue create with sequential issue URLs, an issue's
// database id as 9000 plus its number, and the sub-issue list from the links
// it has seen, recording every call.
type fakeGH struct {
	next   int
	calls  [][]string
	failAt int          // fail the Nth call (1-based); 0 never fails
	linked map[int]bool // sub-issue numbers linked so far
}

func (f *fakeGH) run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	if f.failAt > 0 && len(f.calls) == f.failAt {
		return "", errors.New("gh: rate limited")
	}
	if f.linked == nil {
		f.linked = map[int]bool{}
	}
	switch {
	case args[0] == "issue" && args[1] == "create":
		f.next++
		return fmt.Sprintf("https://github.com/o/r/issues/%d\n", 100+f.next), nil
	case args[0] == "api" && slices.Contains(args, "POST"):
		var id int
		_, _ = fmt.Sscanf(args[len(args)-1], "sub_issue_id=%d", &id)
		f.linked[id-9000] = true
	case args[0] == "api" && strings.HasSuffix(args[1], "/sub_issues"):
		var out []string
		for n := range f.linked {
			out = append(out, fmt.Sprint(n))
		}
		return strings.Join(out, "\n"), nil
	case args[0] == "api":
		return "9" + args[1][strings.LastIndex(args[1], "/")+1:], nil
	}
	return "", nil
}

func (f *fakeGH) creates() []string {
	var out []string
	for _, c := range f.calls {
		if c[0] == "issue" && c[1] == "create" {
			out = append(out, c[slices.Index(c, "--title")+1])
		}
	}
	return out
}

func (f *fakeGH) links() []string {
	var out []string
	for _, c := range f.calls {
		if c[0] == "api" && slices.Contains(c, "POST") {
			out = append(out, c[1]+" "+c[len(c)-1])
		}
	}
	return out
}

func approvedDoc(t *testing.T, d Doc) string {
	t.Helper()
	d.Approved, d.Base = true, "abc"
	return writeDoc(t, d)
}

func TestPushCreatesEpicAndSubIssues(t *testing.T) {
	d := sampleDoc()
	d.Issue, d.Source = 0, "epics/billing.md"
	p := approvedDoc(t, d)
	gh := &fakeGH{}
	got, err := Push(context.Background(), gh.run, p, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Billing rewrite", "Add table", "Wire CLI"}; !slices.Equal(gh.creates(), want) {
		t.Fatalf("created %v, want %v", gh.creates(), want)
	}
	if want := []string{"repos/{owner}/{repo}/issues/101/sub_issues sub_issue_id=9102", "repos/{owner}/{repo}/issues/101/sub_issues sub_issue_id=9103"}; !slices.Equal(gh.links(), want) {
		t.Fatalf("links %v, want %v", gh.links(), want)
	}
	if got.Issue != 101 || !slices.Equal(got.Tasks[0].Issues, []string{"#102"}) || !slices.Equal(got.Tasks[1].Issues, []string{"#103"}) {
		t.Fatalf("doc = %+v", got)
	}
	saved, err := LoadDoc(p)
	if err != nil || saved.Issue != 101 || !slices.Equal(saved.Tasks[1].Issues, []string{"#103"}) || !saved.Approved {
		t.Fatalf("saved = %+v, %v", saved, err)
	}
	// Each sub-issue says what it is part of and how to tell it's done.
	for _, c := range gh.calls {
		if c[0] == "issue" && c[slices.Index(c, "--title")+1] == "Wire CLI" {
			body := c[slices.Index(c, "--body")+1]
			for _, want := range []string{"Part of #101", "Add the", "internal/cli/**", "- [ ] saddle x works", "after store (#102)"} {
				if !strings.Contains(body, want) {
					t.Errorf("sub-issue body lacks %q:\n%s", want, body)
				}
			}
		}
	}
}

func TestPushReusesEpicIssueAndRepo(t *testing.T) {
	d := sampleDoc() // its epic came from gh:#12
	d.Repo = "acme/widgets"
	p := approvedDoc(t, d)
	gh := &fakeGH{}
	if _, err := Push(context.Background(), gh.run, p, nil, 0); err != nil {
		t.Fatal(err)
	}
	if want := []string{"Add table", "Wire CLI"}; !slices.Equal(gh.creates(), want) {
		t.Fatalf("created %v, want %v", gh.creates(), want)
	}
	for _, c := range gh.calls {
		if c[0] == "issue" && !slices.Contains(c, "-R") {
			t.Errorf("issue call without -R acme/widgets: %v", c)
		}
	}
	if l := gh.links(); len(l) != 2 || !strings.HasPrefix(l[0], "repos/acme/widgets/issues/12/sub_issues") {
		t.Fatalf("links = %v", l)
	}
	saved, _ := LoadDoc(p)
	if !slices.Equal(saved.Tasks[0].Issues, []string{"acme/widgets#101"}) {
		t.Fatalf("issues = %v", saved.Tasks[0].Issues)
	}
}

func TestPushIsResumable(t *testing.T) {
	d := sampleDoc()
	d.Issue = 0
	p := approvedDoc(t, d)
	// Calls: create epic, list sub-issues, create store, id store, link
	// store, create cli <- fails.
	gh := &fakeGH{failAt: 6}
	if _, err := Push(context.Background(), gh.run, p, nil, 0); err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("err = %v", err)
	}
	saved, _ := LoadDoc(p)
	if saved.Issue != 101 || !slices.Equal(saved.Tasks[0].Issues, []string{"#102"}) || saved.Tasks[1].Issues != nil {
		t.Fatalf("progress not saved: %+v", saved)
	}
	gh2 := &fakeGH{next: 10, linked: gh.linked}
	got, err := Push(context.Background(), gh2.run, p, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Wire CLI"}; !slices.Equal(gh2.creates(), want) {
		t.Fatalf("re-run created %v, want %v", gh2.creates(), want)
	}
	if !slices.Equal(got.Tasks[1].Issues, []string{"#111"}) {
		t.Fatalf("issues = %v", got.Tasks[1].Issues)
	}
	// Everything pushed: a third run creates and links nothing.
	gh3 := &fakeGH{linked: gh2.linked}
	if _, err := Push(context.Background(), gh3.run, p, nil, 0); err != nil || len(gh3.creates()) != 0 || len(gh3.links()) != 0 {
		t.Fatalf("third run: %v, calls %v", err, gh3.calls)
	}
}

func TestPushLinksIssuesALaterRunFindsUnlinked(t *testing.T) {
	d := sampleDoc()
	d.Issue = 0
	p := approvedDoc(t, d)
	// Calls: create epic, list, create store, id store, link store <- fails.
	gh := &fakeGH{failAt: 5}
	if _, err := Push(context.Background(), gh.run, p, nil, 0); err == nil {
		t.Fatal("want error")
	}
	gh2 := &fakeGH{next: 10, linked: gh.linked}
	if _, err := Push(context.Background(), gh2.run, p, nil, 0); err != nil {
		t.Fatal(err)
	}
	if want := []string{"Wire CLI"}; !slices.Equal(gh2.creates(), want) {
		t.Fatalf("re-run created %v, want %v (store already had an issue)", gh2.creates(), want)
	}
	if !gh2.linked[102] || !gh2.linked[111] {
		t.Fatalf("linked = %v, want store's #102 and cli's #111", gh2.linked)
	}
}

func TestPushNeedsApproval(t *testing.T) {
	p := writeDoc(t, sampleDoc())
	gh := &fakeGH{}
	if _, err := Push(context.Background(), gh.run, p, nil, 0); !errors.Is(err, ErrNotApproved) || len(gh.calls) != 0 {
		t.Fatalf("err = %v, calls = %v", err, gh.calls)
	}
}

func TestIssueNumber(t *testing.T) {
	for in, want := range map[string]int{
		"https://github.com/o/r/issues/42\n": 42,
		"https://github.com/o/r/issues/7":    7,
	} {
		if n, err := issueNumber(in); err != nil || n != want {
			t.Errorf("issueNumber(%q) = %d, %v", in, n, err)
		}
	}
	if _, err := issueNumber("oops"); err == nil {
		t.Error("want error")
	}
}
