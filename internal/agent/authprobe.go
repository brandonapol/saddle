package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/usage"
)

// Auth probes (#798). A credentials file outlives the login it holds: an
// expired or revoked OAuth login keeps its file, so availability reads it as
// logged in. A probe makes one tiny real request through the CLI, so
// saddle doctor can tell a working login from a dead one.

// ErrLoggedOut is a probe whose CLI answered with a login prompt or a 401.
var ErrLoggedOut = errors.New("not logged in")

// ErrNoProbe means saddle has no probe for the adapter.
var ErrNoProbe = errors.New("no login probe for this adapter")

// probePrompt is the request a probe makes: the cheapest one that reaches
// the model.
const probePrompt = "Reply with the single word ok."

// authProbes are the args that send probePrompt in one non-interactive turn.
var authProbes = map[string][]string{
	"claude": {"-p", probePrompt, "--max-turns", "1"},
	"codex":  {"exec", "--skip-git-repo-check", probePrompt},
}

// ProbeTimeout caps one probe.
const ProbeTimeout = 90 * time.Second

// probeRun runs a probe and returns its combined output; tests replace it.
var probeRun = func(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// ProbeAuth makes one real request through adapter name's CLI (cmd, or its
// own binary when ""). It returns nil when the request went through,
// ErrLoggedOut (wrapped, with the CLI's words) when the CLI says its login
// is dead, ErrNoProbe when saddle has none, and any other error when the
// probe itself failed and says nothing about the login.
func ProbeAuth(name, cmd string) error {
	args, ok := authProbes[name]
	if !ok {
		return ErrNoProbe
	}
	if cmd == "" {
		cmd = auths[name].bin
	}
	ctx, cancel := context.WithTimeout(context.Background(), ProbeTimeout)
	defer cancel()
	out, err := probeRun(ctx, cmd, args...)
	if usage.DetectAuthBanner(out) {
		return fmt.Errorf("%w: %s", ErrLoggedOut, lastLine(out))
	}
	if err != nil {
		return fmt.Errorf("probe %s: %v: %s", name, err, lastLine(out))
	}
	return nil
}

// lastLine is the last non-blank line of s, for an error.
func lastLine(s string) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(ls[len(ls)-1])
}
