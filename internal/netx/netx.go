// Package netx holds what saddle's network subprocesses (git push and fetch,
// gh) share: a typed timeout error and a command guard that makes a deadline
// stick (#788).
package netx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// ErrNetTimeout is what a network call that ran past its deadline returns,
// wrapped in a *TimeoutError. It is a wait and retry, like GitHub's rate
// limit, never a failure that latches: the network is down, not the work.
var ErrNetTimeout = errors.New("network call timed out")

// TimeoutError says which call ran past its deadline and what the deadline
// was. It matches ErrNetTimeout.
type TimeoutError struct {
	Op    string        // the command, e.g. "git push origin x"
	After time.Duration // the deadline it ran past; 0 if the caller's
}

func (e *TimeoutError) Error() string {
	if e.After > 0 {
		return fmt.Sprintf("%s: no answer in %s, so it was stopped (network call timed out; it is retried later)", e.Op, e.After)
	}
	return fmt.Sprintf("%s: stopped at the caller's deadline (network call timed out; it is retried later)", e.Op)
}

// Is makes errors.Is(err, ErrNetTimeout) true.
func (e *TimeoutError) Is(target error) bool { return target == ErrNetTimeout }

// WaitDelay is how long Wait gives a killed command's children to let go of
// its stdout and stderr before it closes them. Without it a child that
// outlives a killed git (a stuck ssh, git-remote-https) keeps Wait blocked.
const WaitDelay = 2 * time.Second

// Guard makes ctx's end stop cmd for good: cmd runs in a process group of its
// own, the whole group is killed when ctx ends, and Wait stops waiting on the
// pipes WaitDelay later. cmd must come from exec.CommandContext.
func Guard(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = WaitDelay
}

// Timeout returns the *TimeoutError for op when a command run under ctx
// ended because ctx's deadline passed, and nil otherwise.
func Timeout(ctx context.Context, op string, after time.Duration) error {
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil
	}
	return &TimeoutError{Op: op, After: after}
}

// Duration reads a duration from the environment variable name, for tuning
// a deadline without a rebuild; def when it is unset or unparsable.
func Duration(name string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(name)); err == nil && d > 0 {
		return d
	}
	return def
}
