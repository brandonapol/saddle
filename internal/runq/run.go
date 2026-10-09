package runq

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// RunOptions describe one heavy run.
type RunOptions struct {
	Class  string
	Prio   int
	Label  string    // who is asking; SADDLE_TASK or "pid N" when empty
	Status io.Writer // queue lines go here; nil discards them
	// WaitMax gives up queueing after this long with a *WaitMaxError; 0
	// waits until ctx ends.
	WaitMax time.Duration
	// Signals are forwarded to the command while it runs. One that arrives
	// while still queued drops the request and returns *InterruptedError.
	Signals <-chan os.Signal
	// OnQueued is called once, the first time the run has to wait.
	OnQueued func(Wait)
	// OnStart is called when the command is about to start, with the wait.
	OnStart func(waited time.Duration)
}

// Result describes a finished run.
type Result struct {
	Mode     Mode
	Nested   bool // rode on an outer lease
	Waited   time.Duration
	Held     time.Duration // how long the command ran
	CPU      time.Duration // user+system time of the command and the children it reaped
	MaxRSSKB int64         // peak resident set of the largest process
}

// WaitMaxError is returned when a run queued longer than RunOptions.WaitMax.
type WaitMaxError struct {
	Class  string
	Waited time.Duration
	Last   Wait
}

func (e *WaitMaxError) Error() string {
	holder := "no holder"
	if len(e.Last.Holders) > 0 {
		var hs []string
		for _, h := range e.Last.Holders {
			hs = append(hs, fmt.Sprintf("%s (%s, pid %d, %s)", h.Label, h.Cmd, h.PID, h.Age.Round(time.Second)))
		}
		holder = "holder " + strings.Join(hs, ", ")
	}
	if e.Last.Drained {
		holder += "; the class is drained"
	}
	return fmt.Sprintf("runq: gave up after waiting %s for a %s slot; %s. "+
		"Report this (in your done summary or to the orchestrator); do not retry in a loop.",
		e.Waited.Round(time.Second), e.Class, holder)
}

// InterruptedError is returned when a signal arrived while the run queued.
type InterruptedError struct{ Signal os.Signal }

func (e *InterruptedError) Error() string {
	return fmt.Sprintf("runq: interrupted by %s while queued", e.Signal)
}

// Run waits for a slot in class, runs cmd holding it, and releases it. While
// queued it prints one line to out each time the position or holder changes
// ("queued: position 1 of 2 for go-test, holder t83 (make check, 40s)"), and
// one line when the run starts after a wait. The child gets EnvLease, so
// heavy runs it starts itself ride on this lease instead of queueing behind
// it. Run returns cmd's error.
func (q *Queue) Run(ctx context.Context, class string, prio int, cmd *exec.Cmd, out io.Writer) error {
	_, err := q.RunWith(ctx, RunOptions{Class: class, Prio: prio, Status: out}, cmd)
	return err
}

// RunWith is Run with options and a result. Its error is cmd's (an
// *exec.ExitError for a non-zero exit), a *WaitMaxError, an
// *InterruptedError, ctx's, or the queue's.
func (q *Queue) RunWith(ctx context.Context, o RunOptions, cmd *exec.Cmd) (Result, error) {
	out := o.Status
	if out == nil {
		out = io.Discard
	}
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if o.WaitMax > 0 {
		var c context.CancelFunc
		wctx, c = context.WithTimeout(wctx, o.WaitMax)
		defer c()
	}
	// A signal while queued cancels the wait; the goroutine stops watching
	// once Acquire returns, and from then on signals go to the command.
	var mu sync.Mutex
	var interrupted os.Signal
	waiting := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		select {
		case s := <-o.Signals: // a nil channel never delivers
			mu.Lock()
			interrupted = s
			mu.Unlock()
			cancel()
		case <-waiting:
		}
	}()
	queued := false
	var last Wait
	start := q.opts.Now()
	l, err := q.Acquire(wctx, Request{
		Class: o.Class,
		Prio:  o.Prio,
		Label: o.Label,
		Cmd:   describe(cmd),
		OnWait: func(w Wait) {
			last = w
			if !queued && o.OnQueued != nil {
				o.OnQueued(w)
			}
			queued = true
			fmt.Fprintln(out, w.String())
		},
	})
	close(waiting)
	<-watched
	mu.Lock()
	sig := interrupted
	mu.Unlock()
	if sig != nil {
		if err == nil {
			_ = l.end("canceled") // granted as the signal came: never started
		}
		return Result{}, &InterruptedError{Signal: sig}
	}
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
			return Result{}, &WaitMaxError{Class: o.Class, Waited: q.opts.Now().Sub(start), Last: last}
		}
		return Result{}, err
	}
	res := Result{Mode: l.mode, Nested: l.Nested(), Waited: l.Waited()}
	if l.Nested() {
		res.Mode = ModeEnforce
	}
	if queued {
		fmt.Fprintf(out, "runq: %s slot acquired after %s\n", o.Class, l.Waited().Round(time.Second))
	}
	if o.OnStart != nil {
		o.OnStart(l.Waited())
	}
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	if l.Token() != "" {
		cmd.Env = append(cmd.Env, EnvLease+"="+l.Token())
	}
	dieWithParent(cmd)
	restore := func() {}
	if !l.Nested() && !l.Bypassed() {
		// A nested run inherits the outer run's niceness and scope.
		restore = q.shape(cmd, out)
	}
	began := q.opts.Now()
	runErr := cmd.Start()
	restore()
	if runErr == nil {
		runErr = waitForwarding(cmd, o.Signals)
	}
	res.Held = q.opts.Now().Sub(began)
	if cmd.ProcessState != nil {
		res.CPU, res.MaxRSSKB = rusage(cmd.ProcessState)
	}
	l.cpu, l.maxRSSKB = res.CPU, res.MaxRSSKB
	how := "ok"
	if runErr != nil {
		how = "failed"
	}
	if err := l.end(how); err != nil && runErr == nil {
		return res, err
	}
	return res, runErr
}

// waitForwarding waits for cmd, passing it every signal from sigs.
func waitForwarding(cmd *exec.Cmd, sigs <-chan os.Signal) error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case err := <-done:
			return err
		case s := <-sigs:
			_ = cmd.Process.Signal(s)
		}
	}
}

func describe(cmd *exec.Cmd) string {
	s := strings.Join(cmd.Args, " ")
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}

// Exec runs cmd without the queue, forwarding signals from sigs: the
// fail-open path when the queue itself can't be opened.
func Exec(cmd *exec.Cmd, sigs <-chan os.Signal) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	return waitForwarding(cmd, sigs)
}
