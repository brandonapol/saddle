package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

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

// ShellGate is the GateRun of a shell command in dir, in GateEnviron.
func ShellGate(dir, cmd string) GateRun {
	return func(ctx context.Context, env []string) (string, error) {
		c := exec.CommandContext(ctx, "sh", "-c", cmd)
		c.Dir = dir
		c.Env = append(GateEnviron(os.Environ()), env...)
		out, err := c.CombinedOutput()
		return string(out), err
	}
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
		if res.Err == nil {
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

// gateOnce runs the gate once in a fresh per-run temp dir, removed after.
func (a *App) gateOnce(ctx context.Context, base string, run GateRun) (string, error) {
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", fmt.Errorf("gate tmpdir: %w", err)
	}
	dir, err := os.MkdirTemp(base, "gate-")
	if err != nil {
		// Can't even make the dir: that is the environment too.
		return "cannot create temp dir: " + err.Error(), err
	}
	defer func() { _ = os.RemoveAll(dir) }()
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
