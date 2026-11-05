package forge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/netx"
)

// The forge CLIs. Only this package starts them (see TestNoDirectCLIExec).
const (
	GH   = "gh"
	Glab = "glab"
)

// IsCLI reports whether name is a forge CLI, so a generic exec helper knows
// to hand it to this package.
func IsCLI(name string) bool { return name == GH || name == Glab }

// Output runs the forge CLI name with args in dir and returns its stdout and
// stderr as they were printed. It is the one place saddle starts a forge CLI.
//
// While GitHub's rate limit is spent it returns a *RateLimitError
// (errors.Is ErrRateLimited) without running gh; a gh call that hits the
// limit records it for every saddle process in the repo (see ratelimit.go).
//
// Every call gets a deadline (see CallTimeout); one that runs past it, or
// past ctx's own, returns a *netx.TimeoutError (errors.Is ErrNetTimeout).
func Output(ctx context.Context, dir, name string, args ...string) (stdout, stderr string, err error) {
	if err := held(dir, name); err != nil {
		return "", "", err
	}
	ctx, after, cancel := callContext(ctx, args)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	netx.Guard(cmd)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	if err != nil {
		if terr := netx.Timeout(ctx, opName(name, args), after); terr != nil {
			return out.String(), errb.String(), terr
		}
	}
	if err != nil && name == GH && IsRateLimitMessage(errb.String()) {
		err = tripped(dir, opName(name, args), err)
	}
	return out.String(), errb.String(), err
}

// CombinedOutput runs the forge CLI name with args in dir and returns its
// stdout and stderr interleaved, under the same deadline as Output.
func CombinedOutput(ctx context.Context, dir, name string, args ...string) (string, error) {
	ctx, after, cancel := callContext(ctx, args)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	netx.Guard(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if terr := netx.Timeout(ctx, opName(name, args), after); terr != nil {
			return string(out), terr
		}
	}
	return string(out), err
}

// ErrNetTimeout is what a forge call that ran past its deadline returns. It
// is the same error a timed-out git push or fetch returns (gitx.ErrNetTimeout).
var ErrNetTimeout = netx.ErrNetTimeout

// CallTimeout is the deadline of one forge CLI call, so a gh call on a
// connection that died silently can't hang a watcher (#788). LogTimeout
// replaces it for `run view --log`, which downloads a whole job log.
// SADDLE_FORGE_TIMEOUT and SADDLE_FORGE_LOG_TIMEOUT (Go durations) override
// them; 0 here turns the deadline off.
var (
	CallTimeout = netx.Duration("SADDLE_FORGE_TIMEOUT", 60*time.Second)
	LogTimeout  = netx.Duration("SADDLE_FORGE_LOG_TIMEOUT", 5*time.Minute)
)

// Transient reports whether err is a wait rather than a failure: GitHub's
// rate limit (#764) or a network call that timed out (#788). Callers retry
// later and log nothing; neither ever latches.
func Transient(err error) bool {
	return errors.Is(err, ErrRateLimited) || errors.Is(err, ErrNetTimeout)
}

// callContext is ctx with the deadline a call with args gets, and that
// deadline.
func callContext(ctx context.Context, args []string) (context.Context, time.Duration, context.CancelFunc) {
	d := CallTimeout
	if isLogDownload(args) {
		d = LogTimeout
	}
	if d <= 0 {
		return ctx, 0, func() {}
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	return ctx, d, cancel
}

// isLogDownload reports whether args are `run view … --log` or --log-failed.
func isLogDownload(args []string) bool {
	if len(args) < 2 || args[0] != "run" || args[1] != "view" {
		return false
	}
	return slices.ContainsFunc(args[2:], func(a string) bool {
		return a == "--log" || a == "--log-failed"
	})
}

func opName(name string, args []string) string {
	return name + " " + strings.Join(args[:min(2, len(args))], " ")
}

// Runner runs a forge CLI with args and returns its trimmed stdout. A failure's
// error names the CLI and its first two args, and carries its stderr; what
// it printed to stdout comes back too, since gh prints some answers (pr
// checks while checks are pending, a failed job's log) and still exits
// non-zero.
type Runner func(ctx context.Context, args ...string) (string, error)

// CLI is the Runner for the forge CLI name in dir.
func CLI(dir, name string) Runner {
	return func(ctx context.Context, args ...string) (string, error) {
		out, errb, err := Output(ctx, dir, name, args...)
		if err != nil {
			return strings.TrimSpace(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args[:min(2, len(args))], " "), err, strings.TrimSpace(errb))
		}
		return strings.TrimSpace(out), nil
	}
}
