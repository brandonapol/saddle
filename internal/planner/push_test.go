package planner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/forge"
)

// fakeIssues numbers created issues in sequence, lists the sub-issues it
// has linked, and records every call as "create REPO TITLE", "list REPO N"
// or "link REPO PARENT CHILD".
type fakeIssues struct {
	next    int
	calls   []string
	bodies  map[string]string // title -> body
	failAt  int               // fail the Nth call (1-based); 0 never fails
	linked  map[int]bool      // sub-issue numbers linked so far
	noIssue bool              // create returns no number
}

func (f *fakeIssues) call(c string) error {
	f.calls = append(f.calls, c)
	if f.failAt > 0 && len(f.calls) == f.failAt {
		return errors.New("gh: rate limited")
	}
	if f.linked == nil {
		f.linked = map[int]bool{}
	}
	return nil
}

func (f *fakeIssues) CreateIssue(_ context.Context, i forge.NewIssue) (forge.Issue, error) {
	if err := f.call("create " + i.Repo + " " + i.Title); err != nil {
		return forge.Issue{}, err
	}
	if f.bodies == nil {
		f.bodies = map[string]string{}
	}
	f.bodies[i.Title] = i.Body
	if f.noIssue {
		return forge.Issue{URL: "oops"}, nil
	}
	f.next++
	n := 100 + f.next
	return forge.Issue{Number: n, URL: fmt.Sprintf("https://github.com/o/r/issues/%d", n)}, nil
}

func (f *fakeIssues) SubIssueNumbers(_ context.Context, repo string, n int) ([]int, error) {
	if err := f.call(fmt.Sprintf("list %s %d", repo, n)); err != nil {
		return nil, err
	}
	var out []int
	for n := range f.linked {
		out = append(out, n)
	}
	return out, nil
}

func (f *fakeIssues) AddSubIssue(_ context.Context, repo string, parent, child int) error {
	if err := f.call(fmt.Sprintf("link %s %d %d", repo, parent, child)); err != nil {
		return err
	}
	f.linked[child] = true
	return nil
}

func (f *fakeIssues) creates() []string {
	var out []string
	for _, c := range f.calls {
		if rest, ok := strings.CutPrefix(c, "create "); ok {
			_, title, _ := strings.Cut(rest, " ")
			out = append(out, title)
		}
	}
	return out
}

func (f *fakeIssues) links() []string {
	var out []string
	for _, c := range f.calls {
		if rest, ok := strings.CutPrefix(c, "link "); ok {
			out = append(out, strings.TrimSpace(rest))
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
	gh := &fakeIssues{}
	got, err := Push(context.Background(), gh, p, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Billing rewrite", "Add table", "Wire CLI"}; !slices.Equal(gh.creates(), want) {
		t.Fatalf("created %v, want %v", gh.creates(), want)
	}
	if want := []string{"101 102", "101 103"}; !slices.Equal(gh.links(), want) {
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
	body := gh.bodies["Wire CLI"]
	for _, want := range []string{"Part of #101", "Add the", "internal/cli/**", "- [ ] saddle x works", "after store (#102)"} {
		if !strings.Contains(body, want) {
			t.Errorf("sub-issue body lacks %q:\n%s", want, body)
		}
	}
}

func TestPushReusesEpicIssueAndRepo(t *testing.T) {
	d := sampleDoc() // its epic came from gh:#12
	d.Repo = "acme/widgets"
	p := approvedDoc(t, d)
	gh := &fakeIssues{}
	if _, err := Push(context.Background(), gh, p, nil, 0); err != nil {
		t.Fatal(err)
	}
	if want := []string{"Add table", "Wire CLI"}; !slices.Equal(gh.creates(), want) {
		t.Fatalf("created %v, want %v", gh.creates(), want)
	}
	for _, c := range gh.calls {
		if !strings.Contains(c, " acme/widgets ") {
			t.Errorf("call outside acme/widgets: %q", c)
		}
	}
	if l := gh.links(); len(l) != 2 || !strings.HasPrefix(l[0], "acme/widgets 12 ") {
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
	// Calls: create epic, list sub-issues, create store, link store,
	// create cli <- fails.
	gh := &fakeIssues{failAt: 5}
	if _, err := Push(context.Background(), gh, p, nil, 0); err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("err = %v", err)
	}
	saved, _ := LoadDoc(p)
	if saved.Issue != 101 || !slices.Equal(saved.Tasks[0].Issues, []string{"#102"}) || saved.Tasks[1].Issues != nil {
		t.Fatalf("progress not saved: %+v", saved)
	}
	gh2 := &fakeIssues{next: 10, linked: gh.linked}
	got, err := Push(context.Background(), gh2, p, nil, 0)
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
	gh3 := &fakeIssues{linked: gh2.linked}
	if _, err := Push(context.Background(), gh3, p, nil, 0); err != nil || len(gh3.creates()) != 0 || len(gh3.links()) != 0 {
		t.Fatalf("third run: %v, calls %v", err, gh3.calls)
	}
}

func TestPushLinksIssuesALaterRunFindsUnlinked(t *testing.T) {
	d := sampleDoc()
	d.Issue = 0
	p := approvedDoc(t, d)
	// Calls: create epic, list, create store, link store <- fails.
	gh := &fakeIssues{failAt: 4}
	if _, err := Push(context.Background(), gh, p, nil, 0); err == nil {
		t.Fatal("want error")
	}
	gh2 := &fakeIssues{next: 10, linked: gh.linked}
	if _, err := Push(context.Background(), gh2, p, nil, 0); err != nil {
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
	gh := &fakeIssues{}
	if _, err := Push(context.Background(), gh, p, nil, 0); !errors.Is(err, ErrNotApproved) || len(gh.calls) != 0 {
		t.Fatalf("err = %v, calls = %v", err, gh.calls)
	}
}

func TestPushNeedsAnIssueNumber(t *testing.T) {
	d := sampleDoc()
	d.Issue = 0
	gh := &fakeIssues{noIssue: true}
	if _, err := Push(context.Background(), gh, approvedDoc(t, d), nil, 0); err == nil || !strings.Contains(err.Error(), "want an issue URL") {
		t.Fatalf("err = %v", err)
	}
}
