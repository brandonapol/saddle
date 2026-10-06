package runq

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The tests re-exec this test binary as contender processes: with
// RUNQ_HELPER set, TestMain runs the helper instead of the tests.
func TestMain(m *testing.M) {
	if mode := os.Getenv("RUNQ_HELPER"); mode != "" {
		os.Exit(helperMain(mode))
	}
	os.Exit(m.Run())
}

func helperOpts() Options {
	ms := func(k string, def int) time.Duration {
		n, err := strconv.Atoi(os.Getenv(k))
		if err != nil {
			n = def
		}
		return time.Duration(n) * time.Millisecond
	}
	return Options{Path: os.Getenv("RUNQ_DB"), Heartbeat: ms("RUNQ_HEARTBEAT_MS", 200), StaleAfter: ms("RUNQ_STALE_MS", 5000)}
}

func logLine(format string, a ...any) {
	if p := os.Getenv("RUNQ_LOG"); p != "" {
		f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, format+"\n", a...)
			f.Close()
		}
	}
}

// helperMain modes:
//
//	hold:  acquire RUNQ_CLASS, log start/end, hold RUNQ_HOLD_MS ("forever" waits to be killed)
//	run:   Run RUNQ_CLASS around a child in hold mode (nested lease)
//	churn: acquire and release in a loop until killed
func helperMain(mode string) int {
	q, err := Open(helperOpts())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer func() { _ = q.Close() }()
	class, label := os.Getenv("RUNQ_CLASS"), os.Getenv("RUNQ_LABEL")
	prio, _ := strconv.Atoi(os.Getenv("RUNQ_PRIO"))
	ctx := context.Background()
	switch mode {
	case "hold":
		l, err := q.Acquire(ctx, Request{Class: class, Prio: prio, Label: label, Cmd: "hold",
			OnWait: func(w Wait) { fmt.Println(w.String()) }})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		logLine("start %s %d nested=%v", label, time.Now().UnixNano(), l.Nested())
		fmt.Println("acquired")
		if os.Getenv("RUNQ_HOLD_MS") == "forever" {
			select {}
		}
		n, _ := strconv.Atoi(os.Getenv("RUNQ_HOLD_MS"))
		time.Sleep(time.Duration(n) * time.Millisecond)
		logLine("end %s %d", label, time.Now().UnixNano())
		if err := l.Release(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		return 0
	case "run":
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), "RUNQ_HELPER=hold", "RUNQ_LABEL="+label+"-child")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := q.Run(ctx, class, prio, child, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		return 0
	case "churn":
		for {
			l, err := q.Acquire(ctx, Request{Class: class, Label: label})
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 2
			}
			_ = l.Release()
		}
	}
	return 2
}

// proc is a running helper process.
type proc struct {
	cmd   *exec.Cmd
	lines chan string
	done  chan error
}

func startHelper(t *testing.T, mode string, env ...string) *proc {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "RUNQ_HELPER="+mode)
	cmd.Env = append(cmd.Env, env...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &proc{cmd: cmd, lines: make(chan string, 100), done: make(chan error, 1)}
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			p.lines <- sc.Text()
		}
		close(p.lines)
		p.done <- cmd.Wait()
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		for range p.lines { //nolint:revive // draining
		}
		<-p.done
	})
	return p
}

// expect waits for a stdout line containing want and returns every line seen.
func (p *proc) expect(t *testing.T, want string, timeout time.Duration) []string {
	t.Helper()
	var seen []string
	deadline := time.After(timeout)
	for {
		select {
		case l, ok := <-p.lines:
			if !ok {
				t.Fatalf("helper exited before %q; saw %q", want, seen)
			}
			seen = append(seen, l)
			if strings.Contains(l, want) {
				return seen
			}
		case <-deadline:
			t.Fatalf("no %q within %s; saw %q", want, timeout, seen)
		}
	}
}

func (p *proc) wait(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case err := <-p.done:
		p.done <- err // for Cleanup
		if err != nil {
			t.Fatalf("helper failed: %v", err)
		}
	case <-time.After(timeout):
		t.Fatalf("helper still running after %s", timeout)
	}
}

func (p *proc) kill(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
}

type span struct {
	label      string
	start, end int64
	nested     bool
}

// readLog parses the helpers' start/end lines into spans.
func readLog(t *testing.T, path string) []span {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	var out []span
	idx := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		ts, _ := strconv.ParseInt(f[2], 10, 64)
		switch f[0] {
		case "start":
			idx[f[1]] = len(out)
			out = append(out, span{label: f[1], start: ts, nested: len(f) > 3 && f[3] == "nested=true"})
		case "end":
			out[idx[f[1]]].end = ts
		}
	}
	return out
}

func testOpts(t *testing.T) Options {
	return Options{
		Path:      filepath.Join(t.TempDir(), "runq.db"),
		Heartbeat: 200 * time.Millisecond,
		Getenv:    func(string) string { return "" },
	}
}

func helperEnv(o Options, extra ...string) []string {
	return append([]string{
		"RUNQ_DB=" + o.Path,
		fmt.Sprintf("RUNQ_HEARTBEAT_MS=%d", o.Heartbeat.Milliseconds()),
		"SADDLE_RUNQ=", EnvLease + "=",
	}, extra...)
}
