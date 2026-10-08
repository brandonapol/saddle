package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/brandonapol/saddle/internal/store"
)

// The train's test gate must not blame a branch for the machine it ran on
// (#184). A full /tmp, a disk quota or the OOM killer fails every branch
// alike, and bouncing the task to its producer only burns its attempts. So
// the gate runs with TMPDIR and GOTMPDIR in a per-run dir under a
// disk-backed scratch dir ([train] tmpdir, the user cache dir by default)
// that is removed after the run, day-old Test*/go-build* leftovers there are swept
// first, and output that matches an environment signature is retried
// instead of failing the branch. When the environment stays broken, the
// orchestrator is interrupted with what to free; the branch stays queued.

// EventGateEnv records a gate run that failed on the environment.
const EventGateEnv = "gate_env"

// gateEnvRetries is how many times an environment failure is retried.
const gateEnvRetries = 2

// gateStaleAge is how old a leftover temp dir must be to be swept.
const gateStaleAge = 24 * time.Hour

// GateEnvProblem is an environment failure seen in gate output.
type GateEnvProblem struct {
	Signature string // the matched output, e.g. "disk quota exceeded"
	Free      string // what to free or fix
}

var gateEnvSignatures = []GateEnvProblem{
	{"disk quota exceeded", "disk quota: delete old temp dirs (Test*, go-build*) and build caches"},
	{"no space left on device", "disk space: delete old temp dirs (Test*, go-build*) and build caches"},
	{"disk i/o error", "disk: check free space and the filesystem's health"},
	{"cannot create temp", "temp space: make TMPDIR writable and free space on it"},
	{"cannot create temporary", "temp space: make TMPDIR writable and free space on it"},
	{"signal: killed", "memory: the gate was killed, likely by the OOM killer; stop other heavy jobs"},
	{"cannot allocate memory", "memory: stop other heavy jobs"},
	// Another worktree's golangci-lint, run outside saddle's queue (#271).
	{"parallel golangci-lint is running", "another golangci-lint: let it finish, and run heavy checks one at a time"},
}

// ClassifyGateOutput reports whether gate output shows an environment
// failure rather than the branch's own, and which.
func ClassifyGateOutput(out string) (GateEnvProblem, bool) {
	low := strings.ToLower(out)
	for _, p := range gateEnvSignatures {
		if strings.Contains(low, p.Signature) {
			return p, true
		}
	}
	return GateEnvProblem{}, false
}

// GateRun runs the gate once with env added to its environment.
type GateRun func(ctx context.Context, env []string) (string, error)

// ShellGate is the GateRun of a shell command in dir, in GateEnviron, killed
// with everything it started once it runs past timeout (#269).
func ShellGate(dir, cmd string, timeout time.Duration) GateRun {
	return func(ctx context.Context, env []string) (string, error) {
		return runGroup(ctx, dir, cmd, append(GateEnviron(os.Environ()), env...), timeout)
	}
}

// DefaultGateTimeout is [train] gate_timeout when it is unset.
const DefaultGateTimeout = 20 * time.Minute

// gateKillGrace is how long a gate's group has between SIGTERM and SIGKILL.
const gateKillGrace = 3 * time.Second

// GateTimeout is [train] gate_timeout: how long the train's test and lint
// gates and done's lint gate may run before they are killed. It is read
// from the config files here until config.Train carries it.
func (a *App) GateTimeout() time.Duration {
	var d time.Duration
	paths := []string{filepath.Join(a.Root, ".saddle", "config.toml")}
	if home, err := os.UserConfigDir(); err == nil {
		paths = append([]string{filepath.Join(home, "saddle", "config.toml")}, paths...)
	}
	for _, p := range paths {
		var c struct {
			Train struct {
				GateTimeout time.Duration `toml:"gate_timeout"`
			} `toml:"train"`
		}
		if _, err := toml.DecodeFile(p, &c); err == nil && c.Train.GateTimeout > 0 {
			d = c.Train.GateTimeout
		}
	}
	if d <= 0 {
		return DefaultGateTimeout
	}
	return d
}

// GateTimeoutError is a gate killed for running past its timeout. It is the
// branch's failure, not the environment's: a hung test hangs again.
type GateTimeoutError struct{ After time.Duration }

func (e *GateTimeoutError) Error() string { return fmt.Sprintf("gate timed out after %s", e.After) }

// ErrGateInterrupted is a gate killed because saddle was told to stop. Not
// the branch's fault: it stays queued for the next land.
var ErrGateInterrupted = errors.New("gate interrupted: saddle was told to stop")

// runGroup runs cmd under sh in dir in its own process group, with env
// (nil inherits saddle's). It returns as soon as the shell exits, even when
// something it started still holds its output. When ctx is done or timeout
// passes, or saddle is told to stop, the whole group gets SIGTERM, then
// SIGKILL, before it returns.
func runGroup(ctx context.Context, dir, cmd string, env []string, timeout time.Duration) (string, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Dir, c.Env = dir, env
	// A file, not a pipe: a helper the gate leaves running mustn't hold Wait.
	// With no temp space it is a pipe, and WaitDelay stops the wait.
	var buf bytes.Buffer
	if f, err := os.CreateTemp("", "saddle-gate-*.log"); err == nil {
		defer func() {
			f.Close()
			_ = os.Remove(f.Name())
		}()
		c.Stdout, c.Stderr = f, f
	} else {
		c.Stdout, c.Stderr = &buf, &buf
	}
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGTERM) }
	c.WaitDelay = gateKillGrace
	if err := c.Start(); err != nil {
		return err.Error(), err
	}
	pgid := c.Process.Pid
	stop := watchGroup(pgid)
	err := c.Wait()
	stop()
	if ctx.Err() != nil {
		killGroup(pgid)
	}
	if gateGroups.stopping.Load() {
		killGroup(pgid)
		return "", ErrGateInterrupted
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil // the shell passed; a helper held its output
	}
	out := buf.String()
	if f, ok := c.Stdout.(*os.File); ok {
		b, _ := os.ReadFile(f.Name())
		out = string(b)
	}
	if ctx.Err() == context.DeadlineExceeded {
		te := &GateTimeoutError{After: timeout}
		return strings.TrimRight(out, "\n") + "\n" + te.Error(), te
	}
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	return out, err
}

// killGroup stops process group pgid: SIGTERM, then SIGKILL for whatever is
// left after gateKillGrace.
func killGroup(pgid int) {
	if syscall.Kill(-pgid, syscall.SIGTERM) != nil {
		return // already gone
	}
	for end := time.Now().Add(gateKillGrace); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if syscall.Kill(-pgid, 0) != nil {
			return
		}
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

// gateGroups are the gate process groups running now, so a saddle told to
// stop (Ctrl-C, a tool timeout's SIGTERM) takes them down with it instead
// of orphaning them.
var gateGroups struct {
	sync.Mutex
	pgids map[int]bool
	sigs  chan os.Signal
	// stopping is set once a stop signal arrived, before any group is
	// killed, so a gate that dies of it isn't taken for a red one.
	stopping atomic.Bool
}

// watchGroup registers pgid until the returned func is called.
func watchGroup(pgid int) (stop func()) {
	g := &gateGroups
	g.Lock()
	defer g.Unlock()
	if g.pgids == nil {
		g.pgids = map[int]bool{}
	}
	if len(g.pgids) == 0 {
		g.sigs = make(chan os.Signal, 1)
		for _, sig := range []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
			// An ignored signal (nohup) stays ignored: it mustn't kill the gate.
			if !signal.Ignored(sig) {
				signal.Notify(g.sigs, sig)
			}
		}
		go onStopSignal(g.sigs)
	}
	g.pgids[pgid] = true
	return func() {
		g.Lock()
		defer g.Unlock()
		delete(g.pgids, pgid)
		if len(g.pgids) == 0 && g.sigs != nil {
			signal.Stop(g.sigs)
			close(g.sigs)
			g.sigs = nil
		}
	}
}

// onStopSignal kills every running gate group when saddle is told to stop,
// then lets the signal do what it would have.
func onStopSignal(sigs chan os.Signal) {
	sig, ok := <-sigs
	if !ok {
		return
	}
	g := &gateGroups
	g.stopping.Store(true)
	g.Lock()
	pgids := make([]int, 0, len(g.pgids))
	for p := range g.pgids {
		pgids = append(pgids, p)
	}
	if g.sigs == sigs {
		signal.Stop(sigs)
		g.sigs = nil
		g.pgids = map[int]bool{}
	}
	g.Unlock()
	var wg sync.WaitGroup
	for _, p := range pgids {
		wg.Add(1)
		go func() { defer wg.Done(); killGroup(p) }()
	}
	wg.Wait()
	if s, ok := sig.(syscall.Signal); ok {
		_ = syscall.Kill(os.Getpid(), s)
	}
	// Still here: something else handles the signal and carries on, so
	// later gates must run.
	time.Sleep(2 * gateKillGrace)
	g.stopping.Store(false)
}

// pidAlive reports whether process pid exists.
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// GateEnvResult is how a gate run under RunGateEnv went.
type GateEnvResult struct {
	Output string
	Err    error // nil when the gate passed
	// Env is set when the gate still failed on the environment after its
	// retries: not the branch's fault, so it must not be returned to its
	// producer or count against its attempts.
	Env     *GateEnvProblem
	Retries int
}

// gateUnsetEnv are inherited variables that would make the gate's tests see
// the train's checkout or process instead of their own: git's per-invocation
// repo pointers (set when saddle runs from a hook or a rebase) and the vars
// Claude Code sets for the orchestrator saddle runs the train in. Every
// SADDLE_* var goes too: SADDLE_TASK=t0 and SADDLE_ROOT are the train's.
var gateUnsetEnv = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_PREFIX", "GIT_COMMON_DIR",
	"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE",
	"CLAUDE_PROJECT_DIR", "CLAUDE_PLUGIN_ROOT", "CLAUDE_PLUGIN_DATA",
}

// GateEnviron is environ with what the gate must not inherit removed: a
// branch passes or fails the same under the train as by hand.
func GateEnviron(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "SADDLE_") || slices.Contains(gateUnsetEnv, k) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// GateTmpdir is the scratch dir the gate's temp dirs go in: [train] tmpdir,
// else <user cache dir>/saddle/tmp/<repo hash>. It is never inside the repo:
// a test's temp dir there sits under the repo's .saddle/config.toml, so code
// that walks up looking for a saddle repo (the plugin's saddleRoot) finds the
// real one, and the trust prompt for it fails the gate. A [train] tmpdir
// inside the repo is ignored for the default.
func (a *App) GateTmpdir() string {
	if d := a.Cfg.Train.Tmpdir; d != "" {
		if !filepath.IsAbs(d) {
			d = filepath.Join(a.Root, d)
		}
		if !within(a.Root, d) {
			return filepath.Clean(d)
		}
	}
	sum := sha256.Sum256([]byte(a.Root))
	key := hex.EncodeToString(sum[:])[:12]
	if c, err := os.UserCacheDir(); err == nil && !within(a.Root, c) {
		return filepath.Join(c, "saddle", "tmp", key)
	}
	return filepath.Join(os.TempDir(), "saddle-gate-"+key)
}

// within reports whether p is root or under it.
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// RunGateEnv runs task's gate through run with its temp dirs under
// GateTmpdir, retrying an environment failure up to twice. After the
// retries it interrupts the orchestrator and returns the failure with Env
// set.
func (a *App) RunGateEnv(ctx context.Context, task string, run GateRun) GateEnvResult {
	var res GateEnvResult
	base := a.GateTmpdir()
	sweepStaleTemp(base, time.Now().Add(-gateStaleAge))
	for attempt := 0; ; attempt++ {
		res.Output, res.Err = a.gateOnce(ctx, base, run)
		var timedOut *GateTimeoutError
		if res.Err == nil || errors.As(res.Err, &timedOut) || errors.Is(res.Err, ErrGateInterrupted) {
			return res
		}
		p, env := ClassifyGateOutput(res.Output)
		if !env {
			return res
		}
		if attempt == gateEnvRetries {
			res.Env = &p
			a.Store.Event(task, EventGateEnv, fmt.Sprintf("%s; still failing after %d retries, orchestrator told", p.Signature, attempt))
			if err := a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
				"The test gate for %s failed %d times on the environment, not the branch: %q. Free %s. "+
					"The gate's temp dirs are under %s; leftover Test*/go-build* dirs in /tmp count too. "+
					"%s stays queued and nothing was counted against it; land again once there is room.",
				task, attempt+1, p.Signature, p.Free, base, task)); err != nil {
				res.Output += "\n(orchestrator not notified: " + err.Error() + ")"
			}
			return res
		}
		res.Retries++
		a.Store.Event(task, EventGateEnv, fmt.Sprintf("%s: the environment failed, not the branch; retry %d of %d after sweeping %s",
			p.Signature, res.Retries, gateEnvRetries, base))
		sweepStaleTemp(base, time.Now().Add(-gateStaleAge))
	}
}

// gateOnce runs the gate once in a fresh per-run temp dir, removed after
// along with base when nothing else is left in it.
func (a *App) gateOnce(ctx context.Context, base string, run GateRun) (string, error) {
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", fmt.Errorf("gate tmpdir: %w", err)
	}
	dir, err := os.MkdirTemp(base, "gate-")
	if err != nil {
		// Can't even make the dir: that is the environment too.
		return "cannot create temp dir: " + err.Error(), err
	}
	defer func() {
		_ = os.RemoveAll(dir)
		_ = os.Remove(base) // only if empty: the default lives in the user's cache dir
	}()
	return run(ctx, []string{"TMPDIR=" + dir, "GOTMPDIR=" + dir})
}

// sweepStaleTemp removes Test*, go-build* and leftover gate-* dirs in base
// last modified before cutoff.
func sweepStaleTemp(base string, cutoff time.Time) {
	es, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range es {
		n := e.Name()
		temp := strings.HasPrefix(n, "Test") || strings.HasPrefix(n, "go-build") || strings.HasPrefix(n, "gate-")
		if !e.IsDir() || !temp {
			continue
		}
		if fi, err := e.Info(); err == nil && fi.ModTime().Before(cutoff) {
			_ = os.RemoveAll(filepath.Join(base, n))
		}
	}
}
