package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/runq"
)

// Heavy-run events (#238).
const (
	EventRunQueued   = "run_queued"   // a heavy run had to wait for a slot
	EventRunStarted  = "run_started"  // it started, after waiting this long
	EventRunFinished = "run_finished" // it ended: how, duration, wait, CPU, RSS, mode
	EventRunqReaped  = "runq_reaped"  // saddle up cleared leases left by dead processes
)

// DefaultWaitMax is how long saddle run queues before giving up (exit 75).
const DefaultWaitMax = 30 * time.Minute

// RunqConfigPath is the repo's heavy-run config. It sits next to
// config.toml rather than in a [runq] section of it; config.Config can grow
// a Runq field that names this file once the config package takes it over.
func RunqConfigPath(root string) string { return filepath.Join(root, ".saddle", "runq.toml") }

// Heavy runs CPU-heavy commands through the machine's heavy-run queue
// (internal/runq). It works outside a saddle repo too: Root and Event are
// then empty, and only the user config applies.
type Heavy struct {
	Root   string                        // repo root; "" outside a repo
	Event  func(task, kind, data string) // the activity log; nil for none
	Getenv func(string) string           // os.Getenv when nil
	Warn   io.Writer                     // config and queue problems; discarded when nil
}

// HeavyOpts describe one heavy run.
type HeavyOpts struct {
	Class   string
	Prio    int
	WaitMax time.Duration    // 0: wait_max from config, else DefaultWaitMax; <0: no limit
	Status  io.Writer        // the queued lines
	Signals <-chan os.Signal // forwarded to the command
}

// Heavy is the queue as this repo sees it.
func (a *App) Heavy() Heavy {
	return Heavy{Root: a.Root, Event: a.Store.Event}
}

// RunHeavy runs cmd holding a slot in class at prio, for saddle's own gates
// (the train and pre-publish checks route here in #239). It returns cmd's
// error, or a *runq.WaitMaxError after the configured wait_max.
func (a *App) RunHeavy(ctx context.Context, class string, prio int, cmd *exec.Cmd) error {
	_, err := a.Heavy().Run(ctx, HeavyOpts{Class: class, Prio: prio}, cmd)
	return err
}

func (h Heavy) getenv(k string) string {
	if h.Getenv == nil {
		return os.Getenv(k)
	}
	return h.Getenv(k)
}

func (h Heavy) warn(format string, a ...any) {
	if h.Warn != nil {
		fmt.Fprintf(h.Warn, "saddle runq: "+format+"\n", a...)
	}
}

func (h Heavy) event(kind, data string) {
	if h.Event != nil {
		h.Event(h.getenv("SADDLE_TASK"), kind, data)
	}
}

// Config reads the repo's .saddle/runq.toml, then the user's runq.toml.
func (h Heavy) Config() (runq.Config, error) {
	repo := ""
	if h.Root != "" {
		repo = RunqConfigPath(h.Root)
	}
	return runq.LoadConfig(repo, runq.UserConfigPath(h.getenv))
}

// Open opens the machine's queue with the merged config. A config file that
// doesn't parse is reported to Warn and the defaults apply: a typo must not
// block every heavy run on the machine.
func (h Heavy) Open() (*runq.Queue, runq.Config, error) {
	cfg, err := h.Config()
	if err != nil {
		h.warn("%v; using the defaults", err)
		cfg = runq.Config{}
	}
	o := cfg.Apply(runq.Options{Getenv: h.getenv})
	if p := h.getenv(runq.EnvPath); p != "" {
		o.Path = p
	}
	q, err := runq.Open(o)
	return q, cfg, err
}

// Run waits for a slot, runs cmd and logs run_queued, run_started and
// run_finished. If the queue can't be opened the command runs unqueued
// (fail open) with a warning.
func (h Heavy) Run(ctx context.Context, o HeavyOpts, cmd *exec.Cmd) (runq.Result, error) {
	q, cfg, err := h.Open()
	if err != nil {
		h.warn("%v; running unqueued", err)
		return runq.Result{Mode: runq.ModeOff}, runq.Exec(cmd, o.Signals)
	}
	defer q.Close()
	wait := o.WaitMax
	switch {
	case wait < 0:
		wait = 0
	case wait == 0 && cfg.WaitMax.D > 0:
		wait = cfg.WaitMax.D
	case wait == 0:
		wait = DefaultWaitMax
	}
	logged := q.Mode() != runq.ModeOff
	res, err := q.RunWith(ctx, runq.RunOptions{
		Class:   o.Class,
		Prio:    o.Prio,
		Status:  o.Status,
		WaitMax: wait,
		Signals: o.Signals,
		OnQueued: func(w runq.Wait) {
			h.event(EventRunQueued, fmt.Sprintf("%s: %s", o.Class, strings.TrimPrefix(w.String(), "queued: ")))
		},
		OnStart: func(waited time.Duration) {
			if logged {
				h.event(EventRunStarted, fmt.Sprintf("%s waited %s: %s", o.Class, waited.Round(time.Millisecond), describeCmd(cmd)))
			}
		},
	}, cmd)
	if logged && !res.Nested && res.Held > 0 {
		how := "ok"
		if err != nil {
			how = "failed"
		}
		h.event(EventRunFinished, fmt.Sprintf("%s %s in %s (waited %s, cpu %s, max rss %d kB, %s): %s",
			o.Class, how, res.Held.Round(time.Millisecond), res.Waited.Round(time.Millisecond),
			res.CPU.Round(time.Millisecond), res.MaxRSSKB, res.Mode, describeCmd(cmd)))
	}
	return res, err
}

func describeCmd(cmd *exec.Cmd) string {
	s := strings.Join(cmd.Args, " ")
	if r := []rune(s); len(r) > 80 {
		s = string(r[:79]) + "…"
	}
	return s
}

// CleanStaleLeases clears leases left by processes that died (a crash, a
// SIGKILLed agent) when saddle up starts. Any contender would reap them on
// its next attempt anyway; doing it here keeps status honest from the
// start. Problems are reported, never fatal.
func (a *App) CleanStaleLeases(out io.Writer) {
	h := a.Heavy()
	h.Warn = out
	q, _, err := h.Open()
	if err != nil {
		h.warn("%v", err)
		return
	}
	defer q.Close()
	n, err := q.Reap()
	if err != nil {
		h.warn("reaping stale leases: %v", err)
		return
	}
	if n > 0 {
		fmt.Fprintf(out, "runq: cleared %d stale heavy-run lease(s) left by dead processes\n", n)
		a.Store.Event("", EventRunqReaped, fmt.Sprintf("cleared %d stale lease(s)", n))
	}
}
