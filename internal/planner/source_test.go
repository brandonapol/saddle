package planner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSource(t *testing.T) {
	tests := []struct {
		in   string
		want Source
		err  bool
	}{
		{in: "-", want: Source{Kind: FromStdin}},
		{in: "epics/foo.md", want: Source{Kind: FromFile, Path: "epics/foo.md"}},
		{in: "gh:#12", want: Source{Kind: FromGitHub, Issue: 12}},
		{in: "gh:12", want: Source{Kind: FromGitHub, Issue: 12}},
		{in: "gh:acme/widgets#7", want: Source{Kind: FromGitHub, Repo: "acme/widgets", Issue: 7}},
		{in: "gh:", err: true},
		{in: "gh:#x", err: true},
		{in: "gh:acme#3", err: true},
		{in: "gh:#0", err: true},
		{in: "", err: true},
	}
	for _, tt := range tests {
		got, err := ParseSource(tt.in)
		if tt.err {
			if err == nil {
				t.Errorf("ParseSource(%q) = %+v, want error", tt.in, got)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("ParseSource(%q) = %+v, %v; want %+v", tt.in, got, err, tt.want)
		}
	}
}

func TestLoadEpicFromFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "foo.md")
	if err := os.WriteFile(p, []byte("intro\n\n# Billing rewrite\n\nSplit meters.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e, err := LoadEpic(context.Background(), Source{Kind: FromFile, Path: p}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e.Title != "Billing rewrite" || !strings.Contains(e.Text, "Split meters.") || e.Issue != 0 {
		t.Fatalf("epic = %+v", e)
	}
}

func TestLoadEpicTitleFallsBackToFileName(t *testing.T) {
	p := filepath.Join(t.TempDir(), "billing-rewrite.md")
	if err := os.WriteFile(p, []byte("no heading\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e, err := LoadEpic(context.Background(), Source{Kind: FromFile, Path: p}, nil, nil)
	if err != nil || e.Title != "billing-rewrite" {
		t.Fatalf("epic = %+v, %v", e, err)
	}
}

func TestLoadEpicFromStdin(t *testing.T) {
	e, err := LoadEpic(context.Background(), Source{Kind: FromStdin}, strings.NewReader("# From a pipe\nbody\n"), nil)
	if err != nil || e.Title != "From a pipe" || !strings.Contains(e.Text, "body") {
		t.Fatalf("epic = %+v, %v", e, err)
	}
	if _, err := LoadEpic(context.Background(), Source{Kind: FromStdin}, strings.NewReader("  \n"), nil); err == nil {
		t.Fatal("empty stdin should be an error")
	}
}

func TestLoadEpicFromGitHubIncludesSubIssues(t *testing.T) {
	var gotRepo string
	fetch := func(_ context.Context, repo string, n int) (Issue, error) {
		gotRepo = repo
		return Issue{Number: n, Title: "Planner", Body: "Build it.", URL: "https://github.com/acme/widgets/issues/6",
			Subs: []Issue{{Number: 7, Title: "Epic sources", Body: "Files and stdin."}, {Number: 8, Title: "Schema", State: "CLOSED"}}}, nil
	}
	e, err := LoadEpic(context.Background(), Source{Kind: FromGitHub, Repo: "acme/widgets", Issue: 6}, nil, fetch)
	if err != nil {
		t.Fatal(err)
	}
	if gotRepo != "acme/widgets" || e.Title != "Planner" || e.Issue != 6 || e.Repo != "acme/widgets" {
		t.Fatalf("epic = %+v (repo %q)", e, gotRepo)
	}
	for _, want := range []string{"# Planner", "Build it.", "#7 Epic sources", "Files and stdin.", "#8 Schema (closed)"} {
		if !strings.Contains(e.Text, want) {
			t.Errorf("epic text lacks %q:\n%s", want, e.Text)
		}
	}
}

func TestLoadEpicFromGitHubNeedsFetcher(t *testing.T) {
	if _, err := LoadEpic(context.Background(), Source{Kind: FromGitHub, Issue: 1}, nil, nil); err == nil {
		t.Fatal("want error without a fetcher")
	}
	boom := func(context.Context, string, int) (Issue, error) { return Issue{}, errors.New("boom") }
	if _, err := LoadEpic(context.Background(), Source{Kind: FromGitHub, Issue: 1}, nil, boom); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}
