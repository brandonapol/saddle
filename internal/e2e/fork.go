package e2e

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Each journey runs in its own process (#521). A World sets process env
// (HOME, TMUX_TMPDIR) for in-process saddle calls, so two Worlds can't share
// a process; a process each lets every journey run in parallel with its own
// temp repo and tmux server. The parent test binary re-runs itself with
// -test.run naming one journey and envFork naming it, and that child runs
// the journey as a plain serial test.
const (
	envFork = "E2E_FORK"       // the journey a child process runs
	envBins = "E2E_BINS_DIR"   // the built binaries, so a child doesn't rebuild
	envBudg = "E2E_BUDGET"     // the suite's time budget (a Go duration; 0 is off)
	envRept = "E2E_REPORT"     // a file the slowest-journeys report is appended to
	slowest = 10               // journeys the report lists
	margin  = 20 * time.Second // a child times out this long before its parent
)

// Budget is how long a package's full journey suite may take on a quiet
// machine; Main fails a full run that takes longer. Load raises it (see
// loadScale). E2E_BUDGET overrides it.
const Budget = 400 * time.Second

// Main is the journey packages' TestMain. It isolates temp dirs from any
// saddle repo, builds saddle, the gh shim and the fake agent into *bins
// (once: forked journeys reuse them), runs the tests, then reports the
// slowest journeys and fails a full run that went over Budget.
func Main(m *testing.M, bins *Bins) {
	os.Exit(runMain(m, bins))
}

func runMain(m *testing.M, bins *Bins) int {
	// The train runs this from inside a saddle repo: temp dirs under it
	// would put every World's repo inside the real one.
	base, err := isolatedTemp()
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(base) }()
	_ = os.Setenv("TMPDIR", base)
	_ = os.Setenv("GOTMPDIR", base) // t.TempDir prefers it to TMPDIR

	child := os.Getenv(envFork) != ""
	if dir := os.Getenv(envBins); child && dir != "" {
		*bins = binsIn(dir)
	} else {
		dir, err := os.MkdirTemp("", "e2ebin")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer func() { _ = os.RemoveAll(dir) }()
		if *bins, err = Build(dir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	forkBins = bins.Dir
	// In-process saddle code (the auto-merge tick) runs gh from PATH too.
	_ = os.Setenv("PATH", bins.Dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, k := range []string{"TMUX", "SADDLE_ROOT", "SADDLE_TASK", "SADDLE_REMOTE_DIR", "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"} {
		_ = os.Unsetenv(k)
	}
	if child {
		return m.Run()
	}

	flag.Parse()
	load := sampleLoad()
	defer load.stop()
	if d := flagDuration("test.timeout"); d > margin {
		// go test's own timeout panics with no word on the journeys that
		// already finished: say which were slow first.
		t := time.AfterFunc(d-margin/2, func() {
			fmt.Fprintf(os.Stderr, "e2e: the suite is about to hit its %s timeout\n", d)
			journeys.report(os.Stderr)
		})
		defer t.Stop()
	}
	start := time.Now()
	code := m.Run()
	took := time.Since(start)
	journeys.report(os.Stdout)
	if p := os.Getenv(envRept); p != "" {
		if f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			journeys.report(f)
			_ = f.Close()
		}
	}
	if code == 0 && fullRun() {
		budget, scale := budgetFor(load.stop(), took)
		if budget > 0 && took > budget {
			fmt.Printf("e2e: FAIL: the suite took %s, over its %s budget (%s quiet, x%.2f for load); "+
				"speed up the slowest journeys above (#521)\n", took.Round(time.Second), budget.Round(time.Second), budgetBase(), scale)
			return 1
		}
		fmt.Printf("e2e: the suite took %s of its %s budget\n", took.Round(time.Second), budget.Round(time.Second))
	}
	return code
}

// forkBins is the binaries dir children reuse.
var forkBins string

// Fork runs t's journey in a child process, in parallel with the other
// journeys, and returns true in the parent, which must then return. In the
// child it returns false and the journey runs. Every top-level test in the
// journey packages starts with it:
//
//	if Fork(t) {
//		return
//	}
func Fork(t *testing.T) bool {
	t.Helper()
	if os.Getenv(envFork) == t.Name() {
		return false
	}
	t.Parallel()
	start := time.Now()
	journeys.start(t.Name(), start)
	defer func() { journeys.end(t.Name(), time.Since(start)) }()

	args := []string{"-test.run", childRun(t.Name()), "-test.v", "-test.count=1"}
	if testing.Short() {
		args = append(args, "-test.short")
	}
	if dl, ok := t.Deadline(); ok {
		if left := time.Until(dl) - margin; left > 0 {
			args = append(args, "-test.timeout", left.Round(time.Second).String())
		}
	}
	cmd := exec.Command(os.Args[0], args...)
	// The race runtime sleeps a second at exit unless told not to.
	gorace := strings.TrimSpace(os.Getenv("GORACE") + " atexit_sleep_ms=0")
	cmd.Env = append(os.Environ(), envFork+"="+t.Name(), envBins+"="+forkBins, "GORACE="+gorace)
	out, err := cmd.CombinedOutput()
	t.Logf("journey %s ran in its own process (%s):\n%s", t.Name(), time.Since(start).Round(time.Millisecond), out)
	switch {
	case err != nil:
		t.Fatalf("journey %s failed: %v (its output is above)", t.Name(), err)
	case strings.Contains(string(out), "--- SKIP: "+t.Name()+" "):
		t.Skipf("journey %s skipped", t.Name())
	case !strings.Contains(string(out), "--- PASS: "+t.Name()+" "):
		t.Fatalf("journey %s didn't run in its process: is its name a valid -run pattern?", t.Name())
	}
	return true
}

// childRun is the -test.run pattern for one journey, keeping any subtest
// part of the parent's pattern.
func childRun(name string) string {
	run := "^" + regexp.QuoteMeta(name) + "$"
	if f := flag.Lookup("test.run"); f != nil {
		if _, sub, ok := strings.Cut(f.Value.String(), "/"); ok {
			run += "/" + sub
		}
	}
	return run
}

// fullRun reports whether this run is the whole suite, so its time can be
// held to the budget.
func fullRun() bool {
	for _, f := range []string{"test.run", "test.skip"} {
		if fl := flag.Lookup(f); fl != nil && fl.Value.String() != "" {
			return false
		}
	}
	f := flag.Lookup("test.count")
	return !testing.Short() && (f == nil || f.Value.String() == "1")
}

func flagDuration(name string) time.Duration {
	if f := flag.Lookup(name); f != nil {
		if g, ok := f.Value.(flag.Getter); ok {
			if d, ok := g.Get().(time.Duration); ok {
				return d
			}
		}
	}
	return 0
}

// budgetBase is Budget or E2E_BUDGET's override.
func budgetBase() time.Duration {
	if v, ok := os.LookupEnv(envBudg); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return Budget
}

// budgetFor scales the budget for load other processes put on the machine
// during the run: the mean load less the suite's own CPU use, per core.
func budgetFor(meanLoad float64, took time.Duration) (time.Duration, float64) {
	base := budgetBase()
	scale := loadScale(meanLoad, ownCPU(), took, runtime.NumCPU())
	return time.Duration(float64(base) * scale), scale
}

// loadScale is 1 on a quiet machine and 1 + the others' load per core
// otherwise: with others' load L on C cores the suite gets about C/(C+L) of
// the CPU it would get alone.
func loadScale(meanLoad float64, own, took time.Duration, cpus int) float64 {
	if meanLoad <= 0 || took <= 0 || cpus <= 0 {
		return 1
	}
	others := meanLoad - own.Seconds()/took.Seconds()
	return 1 + max(0, others)/float64(cpus)
}

// ownCPU is the CPU time this process and its waited-for children used.
func ownCPU() time.Duration {
	var d time.Duration
	for _, who := range []int{syscall.RUSAGE_SELF, syscall.RUSAGE_CHILDREN} {
		var ru syscall.Rusage
		if syscall.Getrusage(who, &ru) == nil {
			d += time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
		}
	}
	return d
}

// loadSampler averages the 1-minute load average over the run.
type loadSampler struct {
	once  sync.Once
	done  chan struct{}
	mean  chan float64
	value float64
}

func sampleLoad() *loadSampler {
	s := &loadSampler{done: make(chan struct{}), mean: make(chan float64, 1)}
	go func() {
		var sum float64
		var n int
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			if l, ok := load1(); ok {
				sum, n = sum+l, n+1
			}
			select {
			case <-s.done:
				if n == 0 {
					s.mean <- 0
				} else {
					s.mean <- sum / float64(n)
				}
				return
			case <-tick.C:
			}
		}
	}()
	return s
}

// stop ends sampling and returns the mean load; 0 when it can't be read.
func (s *loadSampler) stop() float64 {
	s.once.Do(func() {
		close(s.done)
		s.value = <-s.mean
	})
	return s.value
}

func load1() (float64, bool) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, false
	}
	l, err := strconv.ParseFloat(f[0], 64)
	return l, err == nil
}

// journeys records how long each journey took.
var journeys = &timings{started: map[string]time.Time{}, took: map[string]time.Duration{}}

type timings struct {
	mu      sync.Mutex
	started map[string]time.Time
	took    map[string]time.Duration
}

func (j *timings) start(name string, at time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.started[name] = at
}

func (j *timings) end(name string, d time.Duration) {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.started, name)
	j.took[name] = max(j.took[name], d) // the slowest of -count runs
}

// report writes the slowest journeys, finished or still running.
func (j *timings) report(w io.Writer) {
	type row struct {
		name    string
		d       time.Duration
		running bool
	}
	var rows []row
	var sum time.Duration
	j.mu.Lock()
	done := len(j.took)
	for n, d := range j.took {
		rows = append(rows, row{n, d, false})
		sum += d
	}
	for n, at := range j.started {
		rows = append(rows, row{n, time.Since(at), true})
	}
	j.mu.Unlock()
	if len(rows) == 0 {
		return
	}
	slices.SortFunc(rows, func(a, b row) int { return int(b.d - a.d) })
	bw := bufio.NewWriter(w)
	fmt.Fprintf(bw, "e2e: %d journeys finished (%s of journey time); the slowest:\n", done, sum.Round(time.Second))
	for _, r := range rows[:min(slowest, len(rows))] {
		state := ""
		if r.running {
			state = " (still running)"
		}
		fmt.Fprintf(bw, "e2e:   %8s  %s%s\n", r.d.Round(100*time.Millisecond), r.name, state)
	}
	_ = bw.Flush()
}

// isolatedTemp makes a temp dir with no saddle repo above it: under the
// temp dir the run was given, else the system's, else the user cache dir.
func isolatedTemp() (string, error) {
	parents := []string{os.TempDir(), "/tmp"}
	if c, err := os.UserCacheDir(); err == nil {
		parents = append(parents, filepath.Join(c, "saddle"))
	}
	for _, p := range parents {
		if os.MkdirAll(p, 0o755) != nil {
			continue
		}
		d, err := os.MkdirTemp(p, "e2e-")
		if err != nil {
			continue
		}
		if d, err = filepath.EvalSymlinks(d); err == nil && EnclosingSaddleRepo(d) == "" {
			return d, nil
		}
		_ = os.RemoveAll(d)
	}
	return "", fmt.Errorf("no temp dir outside a saddle repo under %v", parents)
}

// EnclosingSaddleRepo is the nearest dir at or above dir with a
// .saddle/config.toml, or "": what the plugin's root lookup would find.
func EnclosingSaddleRepo(dir string) string {
	for dir = filepath.Clean(dir); ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, ".saddle", "config.toml")); err == nil {
			return dir
		}
		if dir == filepath.Dir(dir) {
			return ""
		}
	}
}
