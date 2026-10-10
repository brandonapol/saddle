package runq

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// procRoot writes a fake /proc with load1 and, when psi >= 0, CPU PSI.
func procRoot(t *testing.T, load1 string, psi float64) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proc", "pressure"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc", "loadavg"), []byte(load1+" 1.00 1.00 2/300 1234\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if psi >= 0 {
		s := "some avg10=" + strconv.FormatFloat(psi, 'f', 2, 64) + " avg60=0.00 avg300=0.00 total=1\n"
		if err := os.WriteFile(filepath.Join(root, "proc", "pressure", "cpu"), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "runq.toml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestConfigLoadGateDefaults: with no [runq] gate keys, Apply installs a
// LoadGate on /proc at max_load_per_cpu 1.0 and max_cpu_pressure 60, and
// the queue's GateMaxWait defaults to 10m.
func TestConfigLoadGateDefaults(t *testing.T) {
	o := Config{}.Apply(Options{})
	g, ok := o.Gate.(LoadGate)
	if !ok {
		t.Fatalf("gate %#v, want a LoadGate", o.Gate)
	}
	if g.MaxLoadPerCPU != 1.0 || g.MaxPSISomeAvg10 != 60 {
		t.Fatalf("gate %+v", g)
	}
	if _, ok := g.Source.(ProcLoad); !ok {
		t.Fatalf("source %#v", g.Source)
	}
	if withDefaults(o).GateMaxWait != 10*time.Minute {
		t.Fatalf("gate_max_wait %s", withDefaults(o).GateMaxWait)
	}
}

// TestConfigLoadGateKeys: the three [runq] keys reach the gate; the user
// file wins over the repo's; 0 turns a check off, and both off removes the
// gate.
func TestConfigLoadGateKeys(t *testing.T) {
	repo := writeConfig(t, "max_load_per_cpu = 2.5\nmax_cpu_pressure = 40\ngate_max_wait = \"3m\"\n")
	user := writeConfig(t, "max_cpu_pressure = 30\n")
	c, err := LoadConfig(repo, user)
	if err != nil {
		t.Fatal(err)
	}
	o := c.Apply(Options{})
	g := o.Gate.(LoadGate)
	if g.MaxLoadPerCPU != 2.5 || g.MaxPSISomeAvg10 != 30 || o.GateMaxWait != 3*time.Minute {
		t.Fatalf("gate %+v wait %s", g, o.GateMaxWait)
	}
	off := writeConfig(t, "max_load_per_cpu = 0\nmax_cpu_pressure = 0\n")
	if c, err = LoadConfig(repo, off); err != nil {
		t.Fatal(err)
	}
	if o := c.Apply(Options{}); o.Gate != nil {
		t.Fatalf("both checks off still gates: %#v", o.Gate)
	}
	if _, err := LoadConfig(writeConfig(t, "max_load_per_cpu = -1\n")); err == nil {
		t.Fatal("a negative ceiling should be rejected")
	}
}

// TestConfigGateReadsProcRoot: the configured gate reads SADDLE_RUNQ_PROC
// when set (a fixture), holds on a saturated fake box, and fails open when
// the probe can't be read (no /proc/loadavg, as on macOS).
func TestConfigGateReadsProcRoot(t *testing.T) {
	busy := procRoot(t, "64.00", -1)
	o := Config{}.Apply(Options{Getenv: envOf(map[string]string{EnvProcRoot: busy})})
	ok, why := o.Gate.Admit()
	if ok || !strings.Contains(why, "/cpu > 1.00") {
		t.Fatalf("busy box admitted: %v %q", ok, why)
	}
	pressured := procRoot(t, "0.00", 75)
	o = Config{}.Apply(Options{Getenv: envOf(map[string]string{EnvProcRoot: pressured})})
	if ok, why := o.Gate.Admit(); ok || why != "cpu pressure 75% > 60%" {
		t.Fatalf("pressured box: %v %q", ok, why)
	}
	o = Config{}.Apply(Options{Getenv: envOf(map[string]string{EnvProcRoot: t.TempDir()})})
	if ok, _ := o.Gate.Admit(); !ok {
		t.Fatal("an unreadable probe must fail open")
	}
}

// changingGate holds with a reason whose numbers change on every probe, as
// a real load average does.
type changingGate struct{ n atomic.Int64 }

func (g *changingGate) Admit() (bool, string) {
	n := g.n.Add(1)
	return false, "load " + strconv.FormatInt(n, 10) + ".00/cpu > 1.00"
}

// TestGateHoldPrintsOneLine: while the gate holds, the agent sees one
// "waiting on load" line, not one per probe as the load figure moves, and
// the run starts by itself after gate_max_wait.
func TestGateHoldPrintsOneLine(t *testing.T) {
	o := testOpts(t)
	g := &changingGate{}
	o.Gate = g
	o.GateMaxWait = 600 * time.Millisecond
	o.PollMax = 20 * time.Millisecond
	q := open(t, o)
	var out bytes.Buffer
	start := time.Now()
	if err := q.Run(context.Background(), "go-test", PrioWorker, exec.Command("true"), &out); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < o.GateMaxWait {
		t.Fatalf("started after %s, before gate_max_wait", d)
	}
	if g.n.Load() < 5 {
		t.Fatalf("only %d probes; the test needs the reason to change", g.n.Load())
	}
	if n := strings.Count(out.String(), "waiting on load: load"); n != 1 {
		t.Fatalf("%d waiting-on-load lines:\n%s", n, out.String())
	}
	if !strings.Contains(out.String(), "slot acquired after") {
		t.Fatalf("no start line:\n%s", out.String())
	}
}

// TestGateFromConfigHoldsThenGivesUp: a queue opened from config against a
// saturated fake /proc holds a run with a free slot, says why, and starts
// it after gate_max_wait.
func TestGateFromConfigHoldsThenGivesUp(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, "gate_max_wait = \"400ms\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	base := testOpts(t)
	base.Getenv = envOf(map[string]string{EnvProcRoot: procRoot(t, "512.00", -1)})
	q := open(t, c.Apply(base))
	var out bytes.Buffer
	start := time.Now()
	if err := q.Run(context.Background(), "go-test", PrioWorker, exec.Command("true"), &out); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 400*time.Millisecond {
		t.Fatalf("started after %s", d)
	}
	if !strings.Contains(out.String(), "waiting on load: load ") {
		t.Fatalf("no hold line:\n%s", out.String())
	}
}

// TestObserveModeIgnoresGate: observe mode records and never waits, load
// or no load.
func TestObserveModeIgnoresGate(t *testing.T) {
	o := testOpts(t)
	o.Mode = ModeObserve
	o.Gate = LoadGate{Source: &fakeLoad{l: Load{Load1: 100, CPUs: 1}}, MaxLoadPerCPU: 1}
	q := open(t, o)
	start := time.Now()
	if err := q.Run(context.Background(), "go-test", PrioWorker, exec.Command("true"), nil); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("observe waited %s", d)
	}
}

// TestConfigNiceDefaults: heavy children get nice 10 and idle I/O by
// default; nice = 0 and ionice = false turn them off. The systemd scope is
// off unless asked for.
func TestConfigNiceDefaults(t *testing.T) {
	o := Config{}.Apply(Options{})
	if o.Nice != 10 || !o.IOIdle || o.Scope != "" {
		t.Fatalf("defaults: nice %d ionice %v scope %q", o.Nice, o.IOIdle, o.Scope)
	}
	c, err := LoadConfig(writeConfig(t, "nice = 0\nionice = false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if o := c.Apply(Options{}); o.Nice != 0 || o.IOIdle {
		t.Fatalf("off: nice %d ionice %v", o.Nice, o.IOIdle)
	}
	c, err = LoadConfig(writeConfig(t, "scope = \"systemd\"\ncpu_quota = \"400%\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if o := c.Apply(Options{}); o.Scope != ScopeSystemd || o.CPUWeight != 20 || o.CPUQuota != "400%" {
		t.Fatalf("scope: %q weight %d quota %q", o.Scope, o.CPUWeight, o.CPUQuota)
	}
	for _, bad := range []string{"scope = \"docker\"\n", "nice = 25\n"} {
		if _, err := LoadConfig(writeConfig(t, bad)); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

// niceness is the calling process's niceness as `nice` reports it.
func niceness(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("nice").Output()
	if err != nil {
		t.Skipf("no nice: %v", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestRunNicesChild: with Nice set, the child runs at our niceness + Nice
// (capped at 19); a nested run is not reniced again, since it inherits it.
func TestRunNicesChild(t *testing.T) {
	base := niceness(t)
	o := testOpts(t)
	o.Nice = 10
	q := open(t, o)
	var out bytes.Buffer
	cmd := exec.Command("nice")
	cmd.Stdout = &out
	if err := q.Run(context.Background(), "go-test", PrioWorker, cmd, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(out.String()), strconv.Itoa(min(base+10, 19)); got != want {
		t.Fatalf("child niceness %s, want %s", got, want)
	}
	if cmd.Args[0] != "nice" || len(cmd.Args) != 1 {
		t.Fatalf("caller's cmd.Args changed: %v", cmd.Args)
	}

	outer := acquire(t, q, Request{Class: "go-test"})
	defer func() { _ = outer.Release() }()
	o.Getenv = envOf(map[string]string{EnvLease: outer.Token()})
	nq := open(t, o)
	out.Reset()
	cmd = exec.Command("nice")
	cmd.Stdout = &out
	if err := nq.Run(context.Background(), "go-test", PrioWorker, cmd, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != strconv.Itoa(base) {
		t.Fatalf("nested run reniced: %s, want %d", got, base)
	}
}

// TestRunIOIdle: with IOIdle the child's I/O class is idle (Linux).
func TestRunIOIdle(t *testing.T) {
	if _, err := exec.LookPath("ionice"); err != nil {
		t.Skip("no ionice")
	}
	o := testOpts(t)
	o.IOIdle = true
	q := open(t, o)
	var out bytes.Buffer
	cmd := exec.Command("ionice")
	cmd.Stdout = &out
	if err := q.Run(context.Background(), "go-test", PrioWorker, cmd, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "idle" {
		t.Fatalf("child I/O class %q", got)
	}
}

// TestRunWithoutNiceToolsStillRuns: a missing nice or ionice binary is
// skipped, never fatal.
func TestRunWithoutNiceToolsStillRuns(t *testing.T) {
	o := testOpts(t)
	o.Nice, o.IOIdle = 10, true
	o.LookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	q := open(t, o)
	var out bytes.Buffer
	cmd := exec.Command("sh", "-c", "echo ran")
	cmd.Stdout = &out
	if err := q.Run(context.Background(), "go-test", PrioWorker, cmd, nil); err != nil || out.String() != "ran\n" {
		t.Fatalf("%v %q", err, out.String())
	}
}

// fakeSystemdRun puts a systemd-run on a private lookup that logs its
// arguments and runs the command after "--", and a user systemd socket in
// a fake XDG_RUNTIME_DIR. It returns the args log.
func fakeSystemdRun(t *testing.T, o *Options, socket bool) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + log + "\nwhile [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n"
	bin := filepath.Join(dir, "systemd-run")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(dir, "run")
	if socket {
		if err := os.MkdirAll(filepath.Join(run, "systemd"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(run, "systemd", "private"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	o.LookPath = func(name string) (string, error) {
		if name == "systemd-run" {
			return bin, nil
		}
		return exec.LookPath(name)
	}
	o.Getenv = envOf(map[string]string{"XDG_RUNTIME_DIR": run})
	return log
}

// TestScopeOffByDefault: without scope = "systemd" the child never goes
// through systemd-run, even where it exists.
func TestScopeOffByDefault(t *testing.T) {
	base := testOpts(t)
	base.Getenv = envOf(map[string]string{EnvProcRoot: procRoot(t, "0.00", -1)})
	o := Config{}.Apply(base)
	log := fakeSystemdRun(t, &o, true)
	q := open(t, o)
	if err := q.Run(context.Background(), "go-test", PrioWorker, exec.Command("true"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("systemd-run ran without scope = \"systemd\": %v", err)
	}
}

// TestScopeSystemd: scope = "systemd" runs the child in a user scope with
// CPUWeight and the optional CPUQuota, and the command still runs.
func TestScopeSystemd(t *testing.T) {
	o := testOpts(t)
	o.Scope, o.CPUWeight, o.CPUQuota = ScopeSystemd, 20, "200%"
	log := fakeSystemdRun(t, &o, true)
	q := open(t, o)
	var out bytes.Buffer
	cmd := exec.Command("sh", "-c", "echo inside")
	cmd.Stdout = &out
	if err := q.Run(context.Background(), "go-test", PrioWorker, cmd, nil); err != nil {
		t.Fatal(err)
	}
	if out.String() != "inside\n" {
		t.Fatalf("child output %q", out.String())
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	args := string(b)
	for _, want := range []string{"--user\n", "--scope\n", "CPUWeight=20\n", "CPUQuota=200%\n", "--\n"} {
		if !strings.Contains(args, want) {
			t.Fatalf("systemd-run args missing %q:\n%s", want, args)
		}
	}
}

// TestScopeUnavailableRunsWithout: scope = "systemd" with no reachable user
// systemd runs the child directly and says so once.
func TestScopeUnavailableRunsWithout(t *testing.T) {
	o := testOpts(t)
	o.Scope, o.CPUWeight = ScopeSystemd, 20
	log := fakeSystemdRun(t, &o, false)
	q := open(t, o)
	var status bytes.Buffer
	if err := q.Run(context.Background(), "go-test", PrioWorker, exec.Command("true"), &status); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("systemd-run ran without a user systemd")
	}
	if !strings.Contains(status.String(), "no user systemd") {
		t.Fatalf("status %q", status.String())
	}
}

// addHistory records n finished runs of class, each held for held and
// using cpu.
func addHistory(t *testing.T, q *Queue, class string, n int, held, cpu time.Duration, ended time.Time) {
	t.Helper()
	err := q.tx(func(tx *sql.Tx) error {
		for range n {
			if _, err := tx.Exec(`INSERT INTO history(class, label, cmd, waited_ms, held_ms, ended, how, cpu_ms, max_rss_kb, mode)
				VALUES(?, 't', 'c', 0, ?, ?, 'ok', ?, 0, 'observe')`, class, held.Milliseconds(), ended.UnixMilli(), cpu.Milliseconds()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestAdaptiveSlots: slots = max(1, floor(cores × target_util /
// avg_cores(class))) from the last week of history, recomputed at most once
// a day. Classes without enough runs and drained classes are left alone.
func TestAdaptiveSlots(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	o := testOpts(t)
	o.Now = clock
	q := open(t, o)
	// go-test averages 2 cores, flutter-test 7, e2e has too few runs, and
	// lint (0.5 cores) is drained.
	addHistory(t, q, "go-test", 10, 10*time.Second, 20*time.Second, now.Add(-time.Hour))
	addHistory(t, q, "flutter-test", 10, 10*time.Second, 70*time.Second, now.Add(-time.Hour))
	addHistory(t, q, "e2e", 2, 10*time.Second, 10*time.Second, now.Add(-time.Hour))
	addHistory(t, q, "lint", 10, 10*time.Second, 5*time.Second, now.Add(-time.Hour))
	addHistory(t, q, "sleepy", 10, 100*time.Second, time.Second, now.Add(-time.Hour))
	// Older than a week: ignored.
	addHistory(t, q, "go-test", 50, 10*time.Second, 80*time.Second, now.Add(-8*24*time.Hour))
	for _, c := range []string{"go-test", "flutter-test", "e2e", "sleepy"} {
		if err := q.SetSlots(c, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.Drain("lint"); err != nil {
		t.Fatal(err)
	}

	ao := o
	ao.TargetUtil, ao.CPUs = 0.75, 8
	aq := open(t, ao)
	slots := func(c string) int { return class(t, aq, c).Slots }
	if got := slots("go-test"); got != 3 { // floor(8×0.75/2)
		t.Fatalf("go-test slots %d, want 3", got)
	}
	if got := slots("flutter-test"); got != 1 { // floor(6/7) → max(1, 0)
		t.Fatalf("flutter-test slots %d, want 1", got)
	}
	if got := slots("sleepy"); got != 8 { // 0.01 cores would give 600: capped at cores
		t.Fatalf("sleepy slots %d, want 8", got)
	}
	if slots("e2e") != 1 || slots("lint") != 0 {
		t.Fatalf("e2e %d lint %d", slots("e2e"), slots("lint"))
	}

	// Within a day nothing is recomputed, even if history changes.
	addHistory(t, q, "go-test", 100, 10*time.Second, 60*time.Second, now.Add(-time.Minute))
	_ = open(t, ao)
	if got := slots("go-test"); got != 3 {
		t.Fatalf("recomputed within a day: %d", got)
	}
	now = now.Add(25 * time.Hour)
	_ = open(t, ao)
	if got := slots("go-test"); got != 1 { // (20×10 + 60×100)/(10×110) ≈ 5.6 cores → 1
		t.Fatalf("after a day go-test slots %d, want 1", got)
	}
}

// TestAdaptiveSlotsOffByDefault: the default config leaves slots static.
func TestAdaptiveSlotsOffByDefault(t *testing.T) {
	if o := (Config{}).Apply(Options{}); o.TargetUtil != 0 {
		t.Fatalf("adaptive on by default: %v", o.TargetUtil)
	}
	c, err := LoadConfig(writeConfig(t, "adaptive_slots = true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if o := c.Apply(Options{}); o.TargetUtil != 0.75 {
		t.Fatalf("target_util default %v", o.TargetUtil)
	}
	if c, err = LoadConfig(writeConfig(t, "adaptive_slots = true\ntarget_util = 0.5\n")); err != nil {
		t.Fatal(err)
	}
	if o := c.Apply(Options{}); o.TargetUtil != 0.5 {
		t.Fatalf("target_util %v", o.TargetUtil)
	}
}
