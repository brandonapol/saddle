package cli

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// upProcessStart stands in for when the process started: package init runs
// just after exec, before cobra parses anything.
var upProcessStart = time.Now()

// traceStartupEnv turns on saddle up's startup trace (#635): "1" prints it
// when up exits, a path also appends each stage to that file as it happens.
const traceStartupEnv = "SADDLE_TRACE_STARTUP"

// stageFirstFrame is the stage the TUI's first laid-out frame ends.
const stageFirstFrame = "first frame"

// startupTrace records where saddle up spends its time before and just
// after its first frame. A nil trace records nothing.
type startupTrace struct {
	mu          sync.Mutex
	now         func() time.Time
	start, last time.Time
	painted     bool
	lines       []string
	file        string
}

// newStartupTrace is the trace --trace-startup (flag) or
// SADDLE_TRACE_STARTUP (env) asks for, or nil.
func newStartupTrace(flag bool, env string) *startupTrace {
	file := ""
	switch env {
	case "", "0", "false":
		if !flag {
			return nil
		}
	case "1", "true":
	default:
		file = env
	}
	return &startupTrace{now: time.Now, start: upProcessStart, last: upProcessStart, file: file}
}

// Mark ends a stage of the startup path, which began at the previous Mark.
func (t *startupTrace) Mark(stage string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.record(now, now.Sub(t.last), stage)
	t.last = now
	if stage == stageFirstFrame {
		t.painted = true
	}
}

// Span starts work that runs beside the startup path; the returned func
// ends it.
func (t *startupTrace) Span(stage string) (end func()) {
	if t == nil {
		return func() {}
	}
	t.mu.Lock()
	began := t.now()
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		now := t.now()
		if t.painted {
			stage += " [after first frame]"
		}
		t.record(now, now.Sub(began), stage)
	}
}

// record adds a line; the caller holds mu.
func (t *startupTrace) record(at time.Time, took time.Duration, stage string) {
	line := fmt.Sprintf("%9s %9s  %s", ms(at.Sub(t.start)), ms(took), stage)
	t.lines = append(t.lines, line)
	if t.file == "" {
		return
	}
	f, err := os.OpenFile(t.file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	fmt.Fprintln(f, line)
	_ = f.Close()
}

// Write prints the trace: when each stage ended, counted from process
// start, and how long it took.
func (t *startupTrace) Write(w io.Writer) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	fmt.Fprintln(w, "saddle up startup trace (ms since process start):")
	fmt.Fprintf(w, "%9s %9s  %s\n", "at", "took", "stage")
	for _, l := range t.lines {
		fmt.Fprintln(w, l)
	}
}

func ms(d time.Duration) string { return fmt.Sprintf("%dms", d.Milliseconds()) }
