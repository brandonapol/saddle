package forge

import (
	"context"
	"errors"
	"testing"
)

// TestGitHubAddLabelCreatesKnownLabel (#787): a missing saddle label is
// created with its color, then the add is retried once.
func TestGitHubAddLabelCreatesKnownLabel(t *testing.T) {
	var calls []string
	missing := true
	g := &GitHub{Run: func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, args[0]+" "+args[1])
		if args[0] == "label" {
			missing = false
			return "", nil
		}
		if missing {
			return "", errors.New("gh pr edit: exit status 1: 'needs-human' not found")
		}
		return "", nil
	}}
	if err := g.AddLabel(context.Background(), "1", "needs-human"); err != nil {
		t.Fatal(err)
	}
	want := []string{"pr edit", "label create", "pr edit"}
	if len(calls) != 3 || calls[0] != want[0] || calls[1] != want[1] || calls[2] != want[2] {
		t.Fatalf("calls = %q, want %q", calls, want)
	}
}

func TestGitHubAddLabelLeavesUnknownLabelAlone(t *testing.T) {
	n := 0
	g := &GitHub{Run: func(context.Context, ...string) (string, error) {
		n++
		return "", errors.New("'mine' not found")
	}}
	if err := g.AddLabel(context.Background(), "1", "mine"); err == nil || n != 1 {
		t.Fatalf("err = %v after %d calls, want the error after one", err, n)
	}
}
