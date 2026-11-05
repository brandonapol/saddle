package forge

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
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
func Output(ctx context.Context, dir, name string, args ...string) (stdout, stderr string, err error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	return out.String(), errb.String(), err
}

// CombinedOutput runs the forge CLI name with args in dir and returns its
// stdout and stderr interleaved.
func CombinedOutput(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Runner runs a forge CLI with args and returns its trimmed stdout. A failure's
// error names the CLI and its first two args, and carries its stderr; what
// it printed to stdout comes back too, since gh prints some answers (pr
// checks while checks are pending, a failed job's log) and still exits
// non-zero.
type Runner func(ctx context.Context, args ...string) (string, error)

// InRepo is run pointed at repo (owner/name) with gh's -R, for a forge
// that works on another repo than the checkout's. "" is run unchanged.
func InRepo(run Runner, repo string) Runner {
	if repo == "" {
		return run
	}
	return func(ctx context.Context, args ...string) (string, error) {
		if slices.Contains(args, "-R") {
			return run(ctx, args...)
		}
		return run(ctx, append(slices.Clone(args), "-R", repo)...)
	}
}

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
