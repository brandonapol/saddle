package runq

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// TestDefaultsObserveModeWithDefaultClasses: with no config the queue
// observes (records, never waits) and seeds the built-in class slots.
func TestDefaultsObserveModeWithDefaultClasses(t *testing.T) {
	q := open(t, Options{Path: filepath.Join(t.TempDir(), "runq.db"), Getenv: envOf(nil)})
	if q.Mode() != ModeObserve {
		t.Fatalf("default mode %q, want observe", q.Mode())
	}
	want := map[string]int{"flutter-test": 1, "golangci-lint": 1, "go-test": 2, "e2e": 1, "generic-heavy": 2}
	for c, n := range want {
		if got := q.slotsFor(c); got != n {
			t.Errorf("slots(%s) = %d, want %d", c, got, n)
		}
	}
}

// TestModeFromEnv: SADDLE_RUNQ picks the mode over the configured one.
func TestModeFromEnv(t *testing.T) {
	for env, want := range map[string]Mode{"": ModeEnforce, "off": ModeOff, "observe": ModeObserve, "enforce": ModeEnforce, "on": ModeEnforce, "OBSERVE": ModeObserve} {
		o := testOpts(t)
		o.Mode = ModeEnforce
		o.Getenv = envOf(map[string]string{EnvBypass: env})
		if got := open(t, o).Mode(); got != want {
			t.Errorf("SADDLE_RUNQ=%q: mode %q, want %q", env, got, want)
		}
	}
}

// TestObserveModeNeverWaits: in observe mode a second run of a one-slot
// class starts at once, both show as holders, and history records both with
// no wait.
func TestObserveModeNeverWaits(t *testing.T) {
	o := testOpts(t)
	o.Mode = ModeObserve
	q := open(t, o)
	a := acquire(t, q, Request{Class: "flutter-test", Label: "a"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	b, err := q.Acquire(ctx, Request{Class: "flutter-test", Label: "b"})
	if err != nil {
		t.Fatalf("observe mode waited: %v", err)
	}
	if c := class(t, q, "flutter-test"); len(c.Holders) != 2 || len(c.Waiters) != 0 {
		t.Fatalf("status %+v", c)
	}
	_ = a.Release()
	_ = b.Release()
	rows, err := q.db.Query(`SELECT label, waited_ms, mode FROM history ORDER BY label`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var label, mode string
		var waited int64
		if err := rows.Scan(&label, &waited, &mode); err != nil {
			t.Fatal(err)
		}
		if waited != 0 || mode != "observe" {
			t.Errorf("%s: waited %d mode %q", label, waited, mode)
		}
		got = append(got, label)
	}
	if strings.Join(got, ",") != "a,b" {
		t.Fatalf("history labels %v", got)
	}
}

// TestHistoryRecordsRusage: a run's CPU time and peak RSS land in history.
func TestHistoryRecordsRusage(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("rusage is recorded on Linux")
	}
	q := open(t, testOpts(t))
	cmd := exec.Command("sh", "-c", `i=0; while [ $i -lt 200000 ]; do i=$((i+1)); done`)
	res, err := q.RunWith(context.Background(), RunOptions{Class: "go-test"}, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if res.CPU <= 0 || res.MaxRSSKB <= 0 {
		t.Fatalf("result %+v", res)
	}
	var cpu, rss int64
	if err := q.db.QueryRow(`SELECT cpu_ms, max_rss_kb FROM history`).Scan(&cpu, &rss); err != nil {
		t.Fatal(err)
	}
	if cpu <= 0 || rss <= 0 {
		t.Fatalf("history cpu_ms=%d max_rss_kb=%d", cpu, rss)
	}
}

// TestPrototypeSchemaMigrates: a queue file written by the prototype (no
// rusage or mode columns) is upgraded in place.
func TestPrototypeSchemaMigrates(t *testing.T) {
	o := testOpts(t)
	db, err := sql.Open("sqlite", "file:"+o.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE history(class TEXT NOT NULL, label TEXT NOT NULL, cmd TEXT NOT NULL,
		waited_ms INTEGER NOT NULL, held_ms INTEGER NOT NULL, ended INTEGER NOT NULL, how TEXT NOT NULL);
		INSERT INTO history VALUES('go-test','old','x',0,1000,0,'ok');`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	q := open(t, o)
	_ = acquire(t, q, Request{Class: "go-test"}).Release()
	var n int
	if err := q.db.QueryRow(`SELECT COUNT(*) FROM history WHERE cpu_ms >= 0 AND mode != 'x'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("history rows %d err %v", n, err)
	}
}

// TestWaitLineETAAndHint: the queued line estimates the wait from the
// class's median run and says the run starts by itself.
func TestWaitLineETAAndHint(t *testing.T) {
	q := open(t, testOpts(t))
	for _, ms := range []int{60_000, 120_000, 120_000} {
		if _, err := q.db.Exec(`INSERT INTO history(class, label, cmd, waited_ms, held_ms, ended, how) VALUES('go-test','x','x',0,?,0,'ok')`, ms); err != nil {
			t.Fatal(err)
		}
	}
	h := acquire(t, q, Request{Class: "go-test", Label: "t83", Cmd: "make check"})
	defer func() { _ = h.Release() }()
	lines := make(chan Wait, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = q.Acquire(ctx, Request{Class: "go-test", OnWait: func(w Wait) { lines <- w }}) }()
	w := <-lines
	if w.ETA != 2*time.Minute {
		t.Fatalf("ETA %s, want the 2m median", w.ETA)
	}
	if s := w.String(); !strings.Contains(s, "holder t83 (make check,") || !strings.HasSuffix(s, ", ~2m (starts automatically; this command may take a while)") {
		t.Fatalf("line %q", s)
	}
}

// TestDrainShowsInWaitLine: a drained class keeps its waiters and says so.
func TestDrainShowsInWaitLine(t *testing.T) {
	q := open(t, testOpts(t))
	_ = acquire(t, q, Request{Class: "go-test"}).Release()
	_ = acquire(t, q, Request{Class: "e2e"}).Release()
	if err := q.Drain(""); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"go-test", "e2e"} {
		if s := class(t, q, c).Slots; s != 0 {
			t.Fatalf("%s slots %d after drain", c, s)
		}
	}
	lines := make(chan string, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = q.Acquire(ctx, Request{Class: "go-test", OnWait: func(w Wait) { lines <- w.String() }}) }()
	if l := <-lines; !strings.Contains(l, "drained") || !strings.Contains(l, "saddle runq slots go-test") {
		t.Fatalf("line %q", l)
	}
}

// TestRunWaitMax: --wait-max gives up with an error naming the holder that
// tells the agent not to retry in a loop, and leaves nothing queued.
func TestRunWaitMax(t *testing.T) {
	q := open(t, testOpts(t))
	h := acquire(t, q, Request{Class: "go-test", Label: "t83", Cmd: "make check"})
	defer func() { _ = h.Release() }()
	_, err := q.RunWith(context.Background(), RunOptions{Class: "go-test", WaitMax: 300 * time.Millisecond}, exec.Command("true"))
	var wm *WaitMaxError
	if !errors.As(err, &wm) {
		t.Fatalf("err %v", err)
	}
	if s := err.Error(); !strings.Contains(s, "t83") || !strings.Contains(s, "not retry") {
		t.Fatalf("message %q", s)
	}
	if c := class(t, q, "go-test"); len(c.Waiters) != 0 {
		t.Fatalf("still queued: %+v", c)
	}
}

// TestRunForwardsSignals: a signal delivered while the command runs reaches
// the command, which decides how to exit.
func TestRunForwardsSignals(t *testing.T) {
	q := open(t, testOpts(t))
	dir := t.TempDir()
	ready, got := filepath.Join(dir, "ready"), filepath.Join(dir, "got")
	sigs := make(chan os.Signal, 1)
	errc := make(chan error, 1)
	go func() {
		cmd := exec.Command("sh", "-c", `trap 'echo term > `+got+`; exit 3' TERM; touch `+ready+`; while :; do sleep 0.05; done`)
		_, err := q.RunWith(context.Background(), RunOptions{Class: "go-test", Signals: sigs}, cmd)
		errc <- err
	}()
	waitFor(t, "child ready", func() bool { _, err := os.Stat(ready); return err == nil })
	sigs <- syscall.SIGTERM
	err := <-errc
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("err %v", err)
	}
	if b, _ := os.ReadFile(got); strings.TrimSpace(string(b)) != "term" {
		t.Fatalf("child saw %q", b)
	}
}

// TestRunSignalWhileQueued: a signal while still queued drops the request
// without starting the command.
func TestRunSignalWhileQueued(t *testing.T) {
	q := open(t, testOpts(t))
	h := acquire(t, q, Request{Class: "go-test"})
	defer func() { _ = h.Release() }()
	marker := filepath.Join(t.TempDir(), "ran")
	sigs := make(chan os.Signal, 1)
	errc := make(chan error, 1)
	go func() {
		_, err := q.RunWith(context.Background(), RunOptions{Class: "go-test", Signals: sigs}, exec.Command("touch", marker))
		errc <- err
	}()
	waitFor(t, "queued", func() bool { return len(class(t, q, "go-test").Waiters) == 1 })
	sigs <- syscall.SIGINT
	var ie *InterruptedError
	if err := <-errc; !errors.As(err, &ie) || ie.Signal != syscall.SIGINT {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("command ran after the wait was interrupted")
	}
	if c := class(t, q, "go-test"); len(c.Waiters) != 0 {
		t.Fatalf("still queued: %+v", c)
	}
}

// TestRunCallbacks: OnQueued fires once when the run queues, OnStart when it
// starts, and the result carries the wait.
func TestRunCallbacks(t *testing.T) {
	q := open(t, testOpts(t))
	h := acquire(t, q, Request{Class: "go-test"})
	var mu sync.Mutex
	var queued, started int
	go func() {
		waitFor(t, "queued", func() bool { mu.Lock(); defer mu.Unlock(); return queued > 0 })
		time.Sleep(100 * time.Millisecond)
		_ = h.Release()
	}()
	res, err := q.RunWith(context.Background(), RunOptions{Class: "go-test",
		OnQueued: func(Wait) { mu.Lock(); queued++; mu.Unlock() },
		OnStart:  func(time.Duration) { mu.Lock(); started++; mu.Unlock() },
	}, exec.Command("true"))
	if err != nil {
		t.Fatal(err)
	}
	if queued != 1 || started != 1 || res.Waited < 100*time.Millisecond || res.Mode != ModeEnforce {
		t.Fatalf("queued %d started %d result %+v", queued, started, res)
	}
}

// TestReapCountsDeadLeases: Reap (saddle up's startup cleanup) removes a
// killed process's lease and reports it.
func TestReapCountsDeadLeases(t *testing.T) {
	o := testOpts(t)
	a := startHelper(t, "hold", helperEnv(o, "RUNQ_CLASS=go-test", "RUNQ_LABEL=a", "RUNQ_HOLD_MS=forever")...)
	a.expect(t, "acquired", 10*time.Second)
	a.kill(t)
	q := open(t, o)
	total := 0
	waitFor(t, "the dead lease reaped", func() bool {
		n, err := q.Reap()
		if err != nil {
			t.Fatal(err)
		}
		total += n
		return total > 0
	})
	if total != 1 {
		t.Fatalf("reaped %d", total)
	}
	if n, _ := q.Reap(); n != 0 {
		t.Fatalf("second reap found %d", n)
	}
}

// TestDefaultPathWithoutHome: with neither XDG_STATE_HOME nor HOME the queue
// still lands at one absolute per-user path, so HOME-less shells share it.
func TestDefaultPathWithoutHome(t *testing.T) {
	p := defaultPath(envOf(nil))
	if !filepath.IsAbs(p) || !strings.HasSuffix(p, filepath.Join("saddle", "runq.db")) {
		t.Fatalf("path %q", p)
	}
	if p != defaultPath(envOf(nil)) {
		t.Fatal("not stable")
	}
	if got := defaultPath(envOf(map[string]string{"XDG_STATE_HOME": "/x"})); got != "/x/saddle/runq.db" {
		t.Fatalf("xdg path %q", got)
	}
	if got := defaultPath(envOf(map[string]string{EnvPath: "/y/q.db", "XDG_STATE_HOME": "/x"})); got != "/y/q.db" {
		t.Fatalf("override path %q", got)
	}
}

// TestLoadConfig: the repo file supplies defaults; the user file (machine
// slots) wins; unknown classes come in with their slots.
func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	repo, user := filepath.Join(dir, "repo.toml"), filepath.Join(dir, "user.toml")
	if err := os.WriteFile(repo, []byte(`mode = "enforce"
wait_max = "5m"
[classes.go-test]
slots = 4
match = ["go test*"]
[classes.dart]
slots = 3
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(user, []byte(`heartbeat = "1s"
[classes.go-test]
slots = 1
`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(repo, user, filepath.Join(dir, "missing.toml"))
	if err != nil {
		t.Fatal(err)
	}
	o := c.Apply(Options{})
	if o.Mode != ModeEnforce || o.Heartbeat != time.Second || c.WaitMax.D != 5*time.Minute {
		t.Fatalf("options %+v config %+v", o, c)
	}
	if o.Slots["go-test"] != 1 || o.Slots["dart"] != 3 || o.Slots["flutter-test"] != 1 {
		t.Fatalf("slots %v", o.Slots)
	}
	if got := c.Classes["go-test"].Match; len(got) != 1 || got[0] != "go test*" {
		t.Fatalf("match %v", got)
	}
	if err := os.WriteFile(user, []byte(`mode = "sometimes"`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(user); err == nil || !strings.Contains(err.Error(), user) {
		t.Fatalf("bad mode: %v", err)
	}
}
