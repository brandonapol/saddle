package forge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// rateLimitRepo makes a saddle-initialised dir and a fake gh that fails
// every call except `api rate_limit` with GitHub's rate-limit message,
// counting the calls it served in a file. rateLimit is what
// `gh api rate_limit` prints; empty makes it fail.
func rateLimitRepo(t *testing.T, rateLimit string) (root, count string) {
	t.Helper()
	root = t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".saddle"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".saddle", "config.toml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	count = filepath.Join(bin, "count")
	answer := "exit 1"
	if rateLimit != "" {
		answer = "echo '" + rateLimit + "'; exit 0"
	}
	script := `#!/bin/sh
if [ "$1 $2" = "api rate_limit" ]; then
  ` + answer + `
fi
echo x >> "` + count + `"
echo "GraphQL: API rate limit already exceeded for user ID 1." >&2
exit 1
`
	if err := os.WriteFile(filepath.Join(bin, GH), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	return root, count
}

func calls(t *testing.T, count string) int {
	b, err := os.ReadFile(count)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "x")
}

// TestOutputHoldsGHWhileRateLimited: the first call that hits the limit
// records it; a second call in the window does not run gh and returns
// ErrRateLimited (#764).
func TestOutputHoldsGHWhileRateLimited(t *testing.T) {
	root, count := rateLimitRepo(t, "")
	var notified []RateLimitHold
	OnRateLimited(func(r string, rl RateLimitHold) {
		if r != root {
			t.Errorf("notified root %q, want %q", r, root)
		}
		notified = append(notified, rl)
	})
	t.Cleanup(func() { OnRateLimited(nil) })

	start := time.Now()
	_, _, err := Output(context.Background(), root, GH, "pr", "view", "7")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("first call err = %v, want ErrRateLimited", err)
	}
	var ee interface{ ExitCode() int }
	if !errors.As(err, &ee) {
		t.Fatalf("first call err = %v, want it to still carry gh's exit error", err)
	}
	if until, ok := RateLimitUntil(err); !ok || until.Before(start.Add(14*time.Minute)) {
		t.Fatalf("until = %v, %v; want the 15m fallback when rate_limit fails", until, ok)
	}

	// A worktree under .saddle/worktrees shares the main checkout's marker.
	wt := filepath.Join(root, ".saddle", "worktrees", "t1")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = CLI(wt, GH)(context.Background(), "issue", "list")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("second call err = %v, want ErrRateLimited", err)
	}
	if n := calls(t, count); n != 1 {
		t.Fatalf("gh ran %d times, want 1: the second call must not exec", n)
	}
	if len(notified) != 1 || notified[0].Source != "gh pr view" {
		t.Fatalf("notified %+v, want one window from gh pr view", notified)
	}
	if until, ok := RateLimited(root); !ok || until.IsZero() {
		t.Fatalf("RateLimited = %v, %v; want held", until, ok)
	}

	// Once the window passes, gh runs again.
	rateLimitNow = func() time.Time { return time.Now().Add(time.Hour) }
	t.Cleanup(func() { rateLimitNow = time.Now })
	if _, ok := RateLimited(root); ok {
		t.Fatal("RateLimited after the window, want not held")
	}
	_, _, _ = Output(context.Background(), root, GH, "pr", "view", "7")
	if n := calls(t, count); n != 2 {
		t.Fatalf("gh ran %d times after the window, want 2", n)
	}
}

// TestRateLimitUsesReportedReset takes the spent bucket's reset time from
// gh api rate_limit.
func TestRateLimitUsesReportedReset(t *testing.T) {
	reset := time.Now().Add(20 * time.Minute).Truncate(time.Second)
	root, _ := rateLimitRepo(t, `{"resources":{"core":{"remaining":4000,"reset":1},"graphql":{"remaining":0,"reset":`+
		strconv.FormatInt(reset.Unix(), 10)+`}}}`)
	_, _, err := Output(context.Background(), root, GH, "api", "graphql")
	if until, ok := RateLimitUntil(err); !ok || !until.Equal(reset) {
		t.Fatalf("until = %v, %v; want graphql reset %v", until, ok, reset)
	}
}

// TestOutputOtherFailuresDontTrip: an ordinary gh failure, or glab, leaves
// no marker.
func TestOutputOtherFailuresDontTrip(t *testing.T) {
	fakePATH(t)
	root, _ := rateLimitRepo(t, "")
	fakePATH(t)
	if _, _, err := Output(context.Background(), root, GH, "fail"); err == nil || errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want a plain failure", err)
	}
	if _, ok := RateLimited(root); ok {
		t.Fatal("a plain failure tripped the governor")
	}
}

// A held call's error still reads as a rate limit to checks that match the
// text, like automerge's restack deferral.
func TestRateLimitErrorSaysRateLimit(t *testing.T) {
	err := fmt.Errorf("gh pr view: %w", &RateLimitError{Until: time.Now()})
	if !IsRateLimitMessage(err.Error()) {
		t.Fatalf("%q does not read as a rate limit", err)
	}
}
