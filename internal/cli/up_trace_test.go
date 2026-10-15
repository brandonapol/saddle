package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeClock steps a fixed amount each time it is read.
func fakeClock(step time.Duration) func() time.Time {
	t := time.Unix(0, 0)
	return func() time.Time { t = t.Add(step); return t }
}

func TestStartupTraceOffIsANoOp(t *testing.T) {
	for _, env := range []string{"", "0", "false"} {
		if tr := newStartupTrace(false, env); tr != nil {
			t.Fatalf("env %q: trace on", env)
		}
	}
	var tr *startupTrace
	tr.Mark("x")
	tr.Span("y")()
	var out bytes.Buffer
	tr.Write(&out)
	if out.Len() != 0 {
		t.Fatalf("a nil trace wrote %q", out.String())
	}
}

// #635: --trace-startup prints each stage with when it ended and how long
// it took; background spans say so.
func TestStartupTraceListsStages(t *testing.T) {
	tr := newStartupTrace(true, "")
	tr.now = fakeClock(100 * time.Millisecond)
	tr.start, tr.last = time.Unix(0, 0), time.Unix(0, 0)
	tr.Mark("saddle init")
	tr.Mark("doctor (local)")
	end := tr.Span("doctor (full)")
	tr.Mark(stageFirstFrame)
	end()
	var out bytes.Buffer
	tr.Write(&out)
	got := out.String()
	for _, want := range []string{
		"startup trace",
		"100ms     100ms  saddle init",
		"200ms     100ms  doctor (local)",
		"400ms     200ms  first frame",
		"500ms     200ms  doctor (full) [after first frame]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("trace lacks %q:\n%s", want, got)
		}
	}
}

// SADDLE_TRACE_STARTUP=<path> appends each stage to the file as it happens,
// so a test or the owner can read it while the TUI holds the screen.
func TestStartupTraceFileGetsEachStageAsItHappens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.txt")
	tr := newStartupTrace(false, path)
	if tr == nil {
		t.Fatal("a path did not turn the trace on")
	}
	tr.Mark("saddle init")
	b, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(b), "saddle init") {
		t.Fatalf("file = %q, %v", b, err)
	}
	tr.Mark(stageFirstFrame)
	b, _ = os.ReadFile(path)
	if !strings.Contains(string(b), stageFirstFrame) {
		t.Fatalf("file = %q", b)
	}
}
