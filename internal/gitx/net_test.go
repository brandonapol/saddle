package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIsNetwork(t *testing.T) {
	for args, want := range map[string]bool{
		"push origin x":                 true,
		"fetch --quiet origin main":     true,
		"ls-remote origin refs/heads/x": true,
		"pull":                          true,
		"-c core.editor=true fetch o":   true,
		"rev-parse HEAD":                false,
		"-c remote.push=x rebase main":  false,
		"update-ref -m push refs/x a b": false,
		"":                              false,
	} {
		if got := IsNetwork(strings.Fields(args)); got != want {
			t.Errorf("IsNetwork(%q) = %v, want %v", args, got, want)
		}
	}
}

// stallGit puts a git on PATH that hangs on push behind a child holding its
// pipes, and otherwise prints the network environment it got.
func stallGit(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	script := `#!/bin/sh
for a in "$@"; do
	if [ "$a" = push ]; then sleep 3600; exit 1; fi
	if [ "$a" = config ]; then exit 1; fi
done
echo "$GIT_HTTP_LOW_SPEED_LIMIT|$GIT_HTTP_LOW_SPEED_TIME|$GIT_SSH_COMMAND"
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, k := range []string{"GIT_HTTP_LOW_SPEED_LIMIT", "GIT_HTTP_LOW_SPEED_TIME", "GIT_SSH_COMMAND", "GIT_SSH"} {
		t.Setenv(k, "")
	}
}

// #788: a push on a dead connection blocked for hours. It now stops at
// NetTimeout, children and all, and says it timed out.
func TestRunStalledPushReturnsErrNetTimeout(t *testing.T) {
	stallGit(t)
	old := NetTimeout
	NetTimeout = 300 * time.Millisecond
	t.Cleanup(func() { NetTimeout = old })
	start := time.Now()
	_, err := Run(t.TempDir(), "push", "origin", "x")
	if !errors.Is(err, ErrNetTimeout) {
		t.Fatalf("stalled push = %v, want ErrNetTimeout", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("stalled push took %s over a 300ms deadline", took)
	}
	if !strings.Contains(err.Error(), "git push origin x") {
		t.Fatalf("error %q doesn't name the command", err)
	}
}

// A caller's own deadline counts the same as NetTimeout.
func TestRunCtxCallerDeadlineIsNetTimeout(t *testing.T) {
	stallGit(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := RunCtx(ctx, t.TempDir(), "push", "origin", "x"); !errors.Is(err, ErrNetTimeout) {
		t.Fatalf("push past the caller's deadline = %v, want ErrNetTimeout", err)
	}
}

func TestRunNetworkEnv(t *testing.T) {
	stallGit(t)
	out, err := Run(t.TempDir(), "fetch", "origin")
	if err != nil {
		t.Fatal(err)
	}
	want := "1000|60|ssh -o ServerAliveInterval=15 -o ServerAliveCountMax=4"
	if out != want {
		t.Fatalf("fetch env = %q, want %q", out, want)
	}
	if out, _ := Run(t.TempDir(), "status"); out != "||" {
		t.Fatalf("a local command got network env: %q", out)
	}
	t.Setenv("GIT_SSH_COMMAND", "ssh -i key")
	t.Setenv("GIT_HTTP_LOW_SPEED_TIME", "5")
	if out, _ := Run(t.TempDir(), "fetch", "origin"); out != "1000|5|ssh -i key -o ServerAliveInterval=15 -o ServerAliveCountMax=4" {
		t.Fatalf("fetch env with the user's own = %q, want theirs kept", out)
	}
}
