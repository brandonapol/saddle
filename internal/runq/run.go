package runq

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Run waits for a slot in class, runs cmd holding it, and releases it. While
// queued it prints one line to out each time the position or holder changes
// ("queued: position 1 of 2 for go-test, holder t83 (make check, 40s)"), and
// one line when the run starts after a wait. The child gets EnvLease, so
// heavy runs it starts itself ride on this lease instead of queueing behind
// it. Run returns cmd's error.
func (q *Queue) Run(ctx context.Context, class string, prio int, cmd *exec.Cmd, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	queued := false
	l, err := q.Acquire(ctx, Request{
		Class: class,
		Prio:  prio,
		Cmd:   describe(cmd),
		OnWait: func(w Wait) {
			queued = true
			fmt.Fprintln(out, w.String())
		},
	})
	if err != nil {
		return err
	}
	if queued {
		fmt.Fprintf(out, "runq: %s slot acquired after %s\n", class, l.Waited().Round(time.Second))
	}
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	if l.Token() != "" {
		cmd.Env = append(cmd.Env, EnvLease+"="+l.Token())
	}
	dieWithParent(cmd)
	runErr := cmd.Run()
	how := "ok"
	if runErr != nil {
		how = "failed"
	}
	if err := l.end(how); err != nil && runErr == nil {
		return err
	}
	return runErr
}

func describe(cmd *exec.Cmd) string {
	s := strings.Join(cmd.Args, " ")
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}
