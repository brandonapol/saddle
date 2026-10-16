package forge

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/config"
)

// #648: branch protection on GitHub through gh api.
func TestGitHubBranchProtection(t *testing.T) {
	ctx := context.Background()
	f := &fakeGH{replies: []string{`{"required_status_checks":{"contexts":["test"],"checks":[{"context":"test"},{"context":"lint"}]},
		"required_pull_request_reviews":{"required_approving_review_count":2},"enforce_admins":{"enabled":true},
		"required_signatures":{"enabled":true},"allow_force_pushes":{"enabled":false}}`}}
	p, err := ReadBranchProtection(ctx, f.github(), "main")
	if err != nil {
		t.Fatal(err)
	}
	want := Protection{Branch: "main", Protected: true, RequireCI: true, Checks: []string{"test", "lint"}, Approvals: 2, Admins: true, Signed: true}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("protection = %+v, want %+v", p, want)
	}
	f.want(t, "api repos/{owner}/{repo}/branches/main/protection")

	f = &fakeGH{err: errors.New("gh api: exit status 1: Branch not protected (HTTP 404)")}
	if p, err := f.github().BranchProtection(ctx, "main"); err != nil || p.Protected {
		t.Fatalf("unprotected = %+v, %v; want not protected, no error", p, err)
	}
	f = &fakeGH{err: errors.New("gh api: exit status 1: Resource not accessible (HTTP 403)")}
	if _, err := f.github().BranchProtection(ctx, "main"); err == nil {
		t.Fatal("a 403 read as no error")
	}
}

// #648: a protected branch on GitLab, then the project for "Pipelines must
// succeed" and approvals.
func TestGitLabBranchProtection(t *testing.T) {
	ctx := context.Background()
	f := &fakeGH{replies: []string{
		`{"name":"main","push_access_levels":[{"access_level":0,"access_level_description":"No one"}],
		  "merge_access_levels":[{"access_level":30,"access_level_description":"Developers + Maintainers"}],"allow_force_push":false}`,
		`{"only_allow_merge_if_pipeline_succeeds":true,"approvals_before_merge":1}`,
	}}
	g := &GitLab{Run: f.run, Host: "git.corp"}
	p, err := ReadBranchProtection(ctx, g, "release/1")
	if err != nil {
		t.Fatal(err)
	}
	want := Protection{Branch: "release/1", Protected: true, RequireCI: true, Approvals: 1,
		Push: []string{"No one"}, Merge: []string{"Developers + Maintainers"}}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("protection = %+v, want %+v", p, want)
	}
	f.want(t, "api --hostname git.corp projects/:id/protected_branches/release%2F1", "api --hostname git.corp projects/:id")

	// GitLab Free has no approvals: unknown, not zero.
	f = &fakeGH{replies: []string{`{"name":"main"}`, `{"only_allow_merge_if_pipeline_succeeds":false}`}}
	if p, err := f.gitlab().BranchProtection(ctx, "main"); err != nil || !p.Protected || p.RequireCI || p.Approvals != -1 {
		t.Fatalf("free tier = %+v, %v", p, err)
	}
	f = &fakeGH{err: errors.New("glab api: exit status 1: 404 Not Found")}
	if p, err := f.gitlab().BranchProtection(ctx, "main"); err != nil || p.Protected {
		t.Fatalf("unprotected = %+v, %v; want not protected, no error", p, err)
	}
	f = &fakeGH{err: errors.New("glab api: exit status 1: 403 Forbidden")}
	if _, err := f.gitlab().BranchProtection(ctx, "main"); err == nil {
		t.Fatal("a 403 read as no error")
	}
}

// A forge without the optional reads says ErrUnsupported, so doctor warns.
func TestOptionalReadsUnsupported(t *testing.T) {
	ctx := context.Background()
	none := Unsupported{Reason: "local-only"}
	if _, err := ReadBranchProtection(ctx, none, "main"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("ReadBranchProtection = %v", err)
	}
	if _, err := ReadDefaultBranch(ctx, none); !errors.Is(err, ErrUnsupported) {
		t.Errorf("ReadDefaultBranch = %v", err)
	}
	if _, err := ReadRateLimit(ctx, &GitLab{}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("ReadRateLimit on GitLab = %v", err)
	}
}

func TestDefaultBranchAndRateLimit(t *testing.T) {
	ctx := context.Background()
	f := &fakeGH{replies: []string{"trunk", `{"resources":{"core":{"limit":5000,"remaining":12,"reset":1700000000}}}`}}
	if b, err := ReadDefaultBranch(ctx, f.github()); err != nil || b != "trunk" {
		t.Fatalf("GitHub default branch = %q, %v", b, err)
	}
	rl, err := ReadRateLimit(ctx, f.github())
	if err != nil || rl != (RateLimit{Limit: 5000, Remaining: 12, Reset: time.Unix(1700000000, 0)}) {
		t.Fatalf("rate limit = %+v, %v", rl, err)
	}
	f.want(t, "repo view --json defaultBranchRef -q .defaultBranchRef.name", "api rate_limit")

	f = &fakeGH{replies: []string{`{"default_branch":"main"}`}}
	if b, err := ReadDefaultBranch(ctx, f.gitlab()); err != nil || b != "main" {
		t.Fatalf("GitLab default branch = %q, %v", b, err)
	}
	f.want(t, "api projects/:id")
}

func TestSelectAndOver(t *testing.T) {
	const gh, gl = "git@github.com:o/r.git", "git@gitlab.com:g/r.git"
	for _, tc := range []struct {
		fc   config.Forge
		url  string
		want string
	}{
		{config.Forge{}, gh, config.ForgeGitHub},
		{config.Forge{}, gl, config.ForgeGitLab},
		{config.Forge{}, "", config.ForgeGitHub},
		{config.Forge{Provider: config.ForgeGitLab}, gh, config.ForgeGitLab},
		{config.Forge{CLI: config.ForgeGlab}, gh, config.ForgeGitLab},
		{config.Forge{Provider: config.ForgeGitHub}, gl, config.ForgeGitHub},
		{config.Forge{CLI: config.ForgeNone}, gl, config.ForgeNone},
	} {
		if got := Select(tc.fc, tc.url); got != tc.want {
			t.Errorf("Select(%+v, %q) = %q, want %q", tc.fc, tc.url, got, tc.want)
		}
		f := Over(tc.fc, tc.url, nil, nil)
		if p := f.Provider(); tc.want != config.ForgeNone && p != tc.want {
			t.Errorf("Over(%+v, %q).Provider() = %q", tc.fc, tc.url, p)
		}
	}
	if args := AuthStatusArgs("h"); !reflect.DeepEqual(args, []string{"auth", "status", "--hostname", "h"}) {
		t.Errorf("AuthStatusArgs = %q", args)
	}
}
