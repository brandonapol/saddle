package scratch

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// mk makes dir p holding a file of n bytes, last changed age ago.
func mk(t *testing.T, p string, n int, age time.Duration) {
	t.Helper()
	must(t, os.MkdirAll(p, 0o755))
	f := filepath.Join(p, "f")
	must(t, os.WriteFile(f, make([]byte, n), 0o644))
	at := time.Now().Add(-age)
	must(t, os.Chtimes(f, at, at))
	must(t, os.Chtimes(p, at, at))
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func noHolders() []string { return nil }

// The sweep removes stale saddle-prefixed dirs and stale root entries,
// keeps fresh ones, never touches what isn't saddle's in the OS temp dir
// and keeps what a live process holds.
func TestSweepRemovesOnlyStaleUnheldSaddleEntries(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root, tmp := filepath.Join(base, "root"), filepath.Join(base, "tmp")
	old := 2 * time.Hour
	for _, p := range []string{"go-build123", "e2ebin9", "saddle-gate-x"} {
		mk(t, filepath.Join(tmp, p), 10, old)
	}
	mk(t, filepath.Join(tmp, "go-build-fresh"), 10, time.Minute)
	mk(t, filepath.Join(tmp, "swiftchain"), 10, old) // the owner's project
	mk(t, filepath.Join(tmp, "TestSomething1"), 10, old)
	mk(t, filepath.Join(tmp, "saddle-held"), 10, old)
	mk(t, filepath.Join(root, "go-build5"), 10, old)
	mk(t, filepath.Join(root, "TestX"), 10, old)
	mk(t, filepath.Join(root, "fresh"), 10, 0)
	key := RepoKey("/some/repo")
	mk(t, filepath.Join(root, key, "gate-1"), 10, old)
	mk(t, filepath.Join(root, key, "gate-2"), 10, old)
	// A symlink with a saddle name pointing at the owner's files.
	must(t, os.Symlink(filepath.Join(tmp, "swiftchain"), filepath.Join(tmp, "saddle-link")))

	held := []string{filepath.Join(tmp, "saddle-held", "f"), filepath.Join(root, key, "gate-2")}
	res := Sweep(Options{Root: root, OSTemp: tmp, Holders: func() []string { return held }})

	for _, gone := range []string{"tmp/go-build123", "tmp/e2ebin9", "tmp/saddle-gate-x", "root/go-build5", "root/TestX", "root/" + key + "/gate-1"} {
		if exists(filepath.Join(base, gone)) {
			t.Errorf("%s is stale and saddle's: want it removed", gone)
		}
	}
	for _, kept := range []string{"tmp/go-build-fresh", "tmp/swiftchain", "tmp/swiftchain/f", "tmp/TestSomething1", "tmp/saddle-held", "tmp/saddle-link", "root/fresh", "root/" + key + "/gate-2", "root/" + key} {
		if !exists(filepath.Join(base, kept)) {
			t.Errorf("%s must be kept", kept)
		}
	}
	if len(res.Removed) != 6 || res.Freed != 60 {
		t.Errorf("removed %d entries, %d bytes; want 6, 60: %+v", len(res.Removed), res.Freed, res.Removed)
	}
	reasons := map[string]string{}
	for _, e := range res.Kept {
		reasons[filepath.Base(e.Path)] = e.Reason
	}
	if !strings.Contains(reasons["saddle-held"], "held") || !strings.Contains(reasons["gate-2"], "held") || !strings.Contains(reasons["go-build-fresh"], "fresh") {
		t.Errorf("kept reasons: %v", reasons)
	}
	if _, ok := reasons["swiftchain"]; ok {
		t.Error("the owner's dir must not even be a candidate")
	}
}

func TestSweepDryRunRemovesNothing(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	mk(t, filepath.Join(base, "tmp", "saddle-x"), 5, 2*time.Hour)
	res := Sweep(Options{Root: filepath.Join(base, "root"), OSTemp: filepath.Join(base, "tmp"), DryRun: true, Holders: noHolders})
	if len(res.Removed) != 1 || !exists(filepath.Join(base, "tmp", "saddle-x")) {
		t.Fatalf("dry run: %+v", res)
	}
}

// A dir a running process works in is held: the real /proc scan sees it.
func TestProcHoldersSeesALiveProcess(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/proc/self/cwd"); err != nil {
		t.Skip("no /proc")
	}
	base := t.TempDir()
	dir := filepath.Join(base, "saddle-busy")
	mk(t, dir, 1, 2*time.Hour)
	cmd := exec.Command("sleep", "30")
	cmd.Dir = dir
	must(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	res := Sweep(Options{Root: filepath.Join(base, "root"), OSTemp: base})
	if !exists(dir) || len(res.Kept) != 1 || !strings.Contains(res.Kept[0].Reason, "held") {
		t.Fatalf("a live process's cwd was swept: %+v", res)
	}
}

// The root under the OS temp dir (the fallback) is never a candidate.
func TestCandidatesSkipTheRootItself(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "saddle-tmp-1000")
	mk(t, root, 1, 2*time.Hour)
	if c := Candidates(root, tmp); slices.Contains(c, root) {
		t.Fatalf("root is a candidate: %v", c)
	}
}

func TestLoadConfig(t *testing.T) {
	repo := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", "/home/u")
	if c := LoadConfig(repo); c.Root() != DefaultRoot() || c.Age() != DefaultMaxAge || c.LowFree() != DefaultLowFreePct {
		t.Fatalf("defaults: %+v root %s", c, c.Root())
	}
	must(t, os.MkdirAll(filepath.Join(repo, ".saddle"), 0o755))
	must(t, os.WriteFile(filepath.Join(repo, ".saddle", "config.toml"),
		[]byte("[tmp]\ndir = \"~/scratch\"\nmax_age = \"30m\"\nlow_free_pct = -1\n"), 0o644))
	c := LoadConfig(repo)
	if c.Root() != "/home/u/scratch" || c.Age() != 30*time.Minute || c.LowFree() != 0 {
		t.Fatalf("configured: %+v", c)
	}
}

func TestUsePointsTempAtRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "r")
	t.Setenv("TMPDIR", "/tmp")
	t.Setenv("GOTMPDIR", "")
	must(t, Use(root))
	if os.Getenv("TMPDIR") != root || os.Getenv("GOTMPDIR") != root || os.TempDir() != root {
		t.Fatalf("TMPDIR %q GOTMPDIR %q", os.Getenv("TMPDIR"), os.Getenv("GOTMPDIR"))
	}
	run := filepath.Join(root, "gate-1")
	t.Setenv("TMPDIR", run)
	must(t, Use(root))
	if os.Getenv("TMPDIR") != run {
		t.Fatalf("a per-run dir under root must be kept, got %q", os.Getenv("TMPDIR"))
	}
}

// fakeDisk is a tmpfs-like filesystem of cap bytes holding only dir: free
// space is what dir's files leave.
func fakeDisk(dir string, capBytes int64) SpaceFunc {
	return func(string) (Space, error) {
		used, _ := size(dir)
		return Space{Path: dir, Free: uint64(max(capBytes-used, 0)), Total: uint64(capBytes)}, nil
	}
}

// Low free space sweeps with the shorter age threshold; when that frees
// enough nothing else happens, and when it doesn't, the biggest entries are
// named, saddle's or not.
func TestRespondSweepsHarderUnderPressure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mk(t, filepath.Join(root, "go-build-20m"), 900, 20*time.Minute) // under 60m, over 5m
	disk := fakeDisk(root, 1000)
	p, err := Respond(Options{Root: root, Holders: noHolders}, 15, disk)
	must(t, err)
	if p.Before.FreePct() >= 15 || p.Swept == nil || p.Low() || exists(filepath.Join(root, "go-build-20m")) {
		t.Fatalf("pressure sweep: %+v", p)
	}

	mk(t, filepath.Join(root, "go-build-now"), 950, 0)
	p, err = Respond(Options{Root: root, Holders: noHolders}, 15, disk)
	must(t, err)
	if !p.Low() || !strings.Contains(p.Message(), "go-build-now") || !strings.Contains(p.Message(), "saddle's") {
		t.Fatalf("still low must name the biggest entry: %q", p.Message())
	}

	p, err = Respond(Options{Root: root, Holders: noHolders}, 0, disk)
	must(t, err)
	if p.Swept != nil || p.Low() {
		t.Fatal("threshold 0 is off")
	}
}

// The journey: a small tmpfs-like scratch dir fills with leftovers of
// killed builds, the pressure response sweeps and recovers, and nobody
// steps in. Then a live build that fills it holds spawns until it ends.
func TestJourneyFullScratchRecoversWithNoHumanStep(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root, tmp := filepath.Join(base, "root"), filepath.Join(base, "tmp")
	for i := range 8 {
		mk(t, filepath.Join(root, "go-build"+string(rune('a'+i))), 120, 10*time.Minute)
	}
	mk(t, filepath.Join(tmp, "swiftchain"), 50, 3*time.Hour)
	disk := fakeDisk(root, 1000)
	if s, _ := disk(root); s.FreePct() >= 15 {
		t.Fatalf("setup: %s", s)
	}
	p, err := Respond(Options{Root: root, OSTemp: tmp, Holders: noHolders}, 15, disk)
	must(t, err)
	if p.Low() || p.After.FreePct() < 99 {
		t.Fatalf("did not recover: %+v", p)
	}
	if !exists(filepath.Join(tmp, "swiftchain", "f")) {
		t.Fatal("the owner's files went")
	}
	live := filepath.Join(root, "go-build-live")
	mk(t, live, 900, 10*time.Minute)
	held := func() []string { return []string{live} }
	p, err = Respond(Options{Root: root, OSTemp: tmp, Holders: held}, 15, disk)
	must(t, err)
	if !p.Low() || !exists(live) {
		t.Fatalf("a live build's dir must be kept and the hold stay: %+v", p)
	}
	p, err = Respond(Options{Root: root, OSTemp: tmp, Holders: noHolders}, 15, disk)
	must(t, err)
	if p.Low() {
		t.Fatalf("once the build ended the next check must recover: %+v", p)
	}
}

func TestStraysFindsFreshSaddleEntriesInOSTemp(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root, tmp := filepath.Join(base, "root"), filepath.Join(base, "tmp")
	mk(t, filepath.Join(tmp, "go-build77"), 1, time.Minute)
	mk(t, filepath.Join(tmp, "saddle-old"), 1, 3*time.Hour)
	mk(t, filepath.Join(tmp, "other"), 1, time.Minute)
	got := Strays(root, tmp, time.Hour, time.Now())
	if len(got) != 1 || filepath.Base(got[0]) != "go-build77" {
		t.Fatalf("strays: %v", got)
	}
}

func TestTopByCount(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, "saddle-app-runq-many"), 0o755))
	for i := range 20 {
		must(t, os.Mkdir(filepath.Join(dir, "saddle-app-runq-many", "d"+string(rune('a'+i))), 0o755))
	}
	mk(t, filepath.Join(dir, "big"), 5000, 0)
	top := Top(dir, 1, true, false)
	if len(top) != 1 || filepath.Base(top[0].Path) != "saddle-app-runq-many" || !top[0].Saddle {
		t.Fatalf("by count: %+v", top)
	}
	top = Top(dir, 1, false, false)
	if filepath.Base(top[0].Path) != "big" || top[0].Saddle {
		t.Fatalf("by bytes: %+v", top)
	}
}

// Tests point OSTemp away from the machine's temp dir so no sweep reaches it.
func TestOSTempHonoursOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvOSTemp, dir)
	if OSTemp() != dir {
		t.Fatalf("OSTemp() = %q, want %q", OSTemp(), dir)
	}
}
