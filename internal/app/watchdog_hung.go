package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// Hung gates (#432): a go test or make check running far past its own
// timeout wedges the task under it, and so the train, while the machine
// looks merely loaded. The watchdog names such a process as the root cause.

// hungMakeCheck is how long a make check may run before it counts as hung;
// it has no timeout of its own.
const hungMakeCheck = 2 * time.Hour

// goTestDefaultTimeout is go test's -timeout when none is given.
const goTestDefaultTimeout = 10 * time.Minute

// Proc is one running process, as the watchdog sees it.
type Proc struct {
	PID     int
	Elapsed time.Duration
	Args    string
	Cwd     string // "" where unknown
}

// HungGate is a gate or test process running far past its timeout.
type HungGate struct {
	Proc
	Timeout time.Duration
	Task    string // the task whose worktree it runs in; "" when none
}

// gateTimeout reports whether args is a go test (or a compiled test
// binary) or a make check, and the time after which it is overdue.
func gateTimeout(args string) (time.Duration, bool) {
	f := strings.Fields(args)
	if len(f) < 2 {
		return 0, false
	}
	base := filepath.Base(f[0])
	switch {
	case base == "make" && strings.Contains(" "+strings.Join(f[1:], " ")+" ", " check "):
		return hungMakeCheck, true
	case base == "go" && f[1] == "test":
		return testTimeout(f[2:], "-timeout", "--timeout")
	case strings.HasSuffix(base, ".test"):
		return testTimeout(f[1:], "-test.timeout", "--test.timeout")
	}
	return 0, false
}

// testTimeout reads the timeout flag from f; go test's default when it
// isn't there, and false when it is 0 (no timeout).
func testTimeout(f []string, names ...string) (time.Duration, bool) {
	d := goTestDefaultTimeout
	for i, a := range f {
		for _, n := range names {
			v, ok := "", false
			switch {
			case a == n && i+1 < len(f):
				v, ok = f[i+1], true
			case strings.HasPrefix(a, n+"="):
				v, ok = strings.TrimPrefix(a, n+"="), true
			}
			if ok {
				p, err := time.ParseDuration(v)
				if err != nil {
					return 0, false
				}
				d = p
			}
		}
	}
	return d, d > 0
}

// HungGates lists the gate and test processes running more than twice
// their timeout, each with the task whose worktree it runs in: go tests
// first (they are the wedged part of a make check), then oldest first.
func HungGates(list func() ([]Proc, error), tasks []store.Task) []HungGate {
	ps, err := list()
	if err != nil {
		return nil
	}
	var out []HungGate
	for _, p := range ps {
		d, ok := gateTimeout(p.Args)
		if !ok || p.Elapsed < 2*d {
			continue
		}
		out = append(out, HungGate{Proc: p, Timeout: d, Task: procTask(p, tasks)})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if mi, mj := isMake(out[i].Args), isMake(out[j].Args); mi != mj {
			return mj
		}
		return out[i].Elapsed > out[j].Elapsed
	})
	return out
}

func isMake(args string) bool {
	f := strings.Fields(args)
	return len(f) > 0 && filepath.Base(f[0]) == "make"
}

// procTask is the task whose worktree p runs in, or names in its args.
func procTask(p Proc, tasks []store.Task) string {
	for _, t := range tasks {
		if t.Worktree == "" {
			continue
		}
		wt := filepath.Clean(t.Worktree)
		if p.Cwd == wt || strings.HasPrefix(p.Cwd, wt+string(filepath.Separator)) || strings.Contains(p.Args, wt) {
			return t.ID
		}
	}
	return ""
}

// hungGateCause is the root cause for the first of hung.
func hungGateCause(hung []HungGate) RootCause {
	h := hung[0]
	owner := "no task's worktree"
	if h.Task != "" {
		owner = "task " + h.Task
	}
	what := fmt.Sprintf("%q (pid %d, under %s) has run %s, far past its %s timeout, so its gate never finishes.",
		h.Args, h.PID, owner, fmtDuration(h.Elapsed), fmtDuration(h.Timeout))
	if n := len(hung) - 1; n > 0 {
		what += fmt.Sprintf(" %d more gate processes are overdue too.", n)
	}
	return RootCause{CauseHungGate, what, fmt.Sprintf("kill %d", h.PID)}
}

// psProcs lists this user's processes with ps, reading each cwd from /proc
// where it can.
func psProcs() ([]Proc, error) {
	out, err := exec.Command("ps", "-o", "pid=,etimes=,args=", "-u", strconv.Itoa(os.Getuid())).Output()
	if err != nil {
		return nil, err
	}
	var ps []Proc
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		secs, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		cwd, _ := os.Readlink("/proc/" + f[0] + "/cwd")
		ps = append(ps, Proc{PID: pid, Elapsed: time.Duration(secs) * time.Second, Args: strings.Join(f[2:], " "), Cwd: cwd})
	}
	return ps, nil
}
