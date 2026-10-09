package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/scratch"
	"github.com/brandonapol/saddle/internal/store"
)

// This package's tests never measure the machine's disk: a spawn must not
// fail because the developer's cache dir is full. Tests of the hold set
// their own (and stay serial).
func init() {
	// Nor sweep the machine's temp dir: tests name their own when they want one.
	_ = os.Setenv(scratch.EnvOSTemp, "/nonexistent/saddle-test-os-temp")
	scratchSpace = func(p string) (scratch.Space, error) {
		return scratch.Space{Path: p, Free: 90, Total: 100}, nil
	}
}

// withScratchDir points the repo's [tmp] dir at a fresh dir and returns it.
func withScratchDir(t *testing.T, a *App) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "scratch")
	must(t, os.MkdirAll(filepath.Join(a.Root, ".saddle"), 0o755))
	cfg := filepath.Join(a.Root, ".saddle", "config.toml")
	b, _ := os.ReadFile(cfg)
	must(t, os.WriteFile(cfg, append(b, []byte("\n[tmp]\ndir = \""+dir+"\"\n")...), 0o644))
	return dir
}

// diskOf is a tmpfs-like filesystem of capBytes holding only dir.
func diskOf(dir string, capBytes int64) scratch.SpaceFunc {
	return func(string) (scratch.Space, error) {
		var used int64
		_ = filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
			if err == nil && !fi.IsDir() {
				used += fi.Size()
			}
			return nil
		})
		return scratch.Space{Path: dir, Free: uint64(max(capBytes-used, 0)), Total: uint64(capBytes)}, nil
	}
}

func fill(t *testing.T, p string, n int, age time.Duration) {
	t.Helper()
	must(t, os.MkdirAll(p, 0o755))
	must(t, os.WriteFile(filepath.Join(p, "f"), make([]byte, n), 0o644))
	at := time.Now().Add(-age)
	must(t, os.Chtimes(filepath.Join(p, "f"), at, at))
	must(t, os.Chtimes(p, at, at))
}

func TestScratchRootAndGateTmpdirFollowTmpDir(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	dir := withScratchDir(t, a)
	if a.ScratchRoot() != dir || a.GateTmpdir() != filepath.Join(dir, scratch.RepoKey(a.Root)) {
		t.Fatalf("root %s gate %s, want under %s", a.ScratchRoot(), a.GateTmpdir(), dir)
	}
}

// Low scratch space sweeps harder before a spawn; when that frees enough the
// spawn goes ahead, and when it doesn't the spawn is held with a banner
// naming what uses the space, until there is room again. Force overrides.
// Serial: it swaps the package's filesystem check.
func TestSpawnHeldWhileScratchStaysLow(t *testing.T) {
	a, _ := setup(t)
	dir := withScratchDir(t, a)
	old := scratchSpace
	scratchSpace = diskOf(dir, 1000)
	t.Cleanup(func() { scratchSpace = old; setScratchHeld(dir, false) })

	fill(t, filepath.Join(dir, "go-build-killed"), 900, 20*time.Minute)
	if _, err := a.Spawn(SpawnReq{Title: "after sweep"}); err != nil {
		t.Fatalf("the pressure sweep should have made room: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "go-build-killed")); !os.IsNotExist(err) {
		t.Fatal("a 20-minute-old build dir survived the pressure sweep")
	}

	fill(t, filepath.Join(dir, "go-build-running"), 950, 0)
	_, err := a.Spawn(SpawnReq{Title: "held"})
	if !errors.Is(err, ErrScratchLow) || !strings.Contains(err.Error(), "go-build-running") {
		t.Fatalf("spawn under low scratch: %v", err)
	}
	if w := a.ScratchWarning(); !strings.Contains(w, "low on space") {
		t.Fatalf("status warning: %q", w)
	}
	ns, err := a.Store.TakeNotices(OrchestratorID, false)
	must(t, err)
	banner := false
	for _, n := range ns {
		banner = banner || n.Kind == store.NoticeAction && strings.Contains(n.Text, "Spawns are on hold") && strings.Contains(n.Text, "go-build-running")
	}
	if !banner {
		t.Fatalf("no hold banner for the orchestrator: %+v", ns)
	}
	if _, err := a.Spawn(SpawnReq{Title: "forced", Force: true}); err != nil {
		t.Fatalf("force must override the hold: %v", err)
	}

	must(t, os.RemoveAll(filepath.Join(dir, "go-build-running")))
	if _, err := a.Spawn(SpawnReq{Title: "room again"}); err != nil {
		t.Fatalf("spawn once there is room: %v", err)
	}
	if a.ScratchWarning() != "" {
		t.Fatal("status still warns with room")
	}
	// Routine news: it goes to the digest, not as an interrupt.
	es, err := a.Store.Events(200)
	must(t, err)
	resumed := false
	for _, e := range es {
		resumed = resumed || strings.Contains(e.Data, "spawns resume")
	}
	if !resumed {
		t.Fatal("the orchestrator never heard spawns resumed")
	}
}

func TestSweepScratchRecordsWhatItRemoved(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	dir := withScratchDir(t, a)
	fill(t, filepath.Join(dir, "TestOld"), 10, 3*time.Hour)
	fill(t, filepath.Join(dir, "TestNew"), 10, 0)
	if res := a.SweepScratch(true, 0); len(res.Removed) != 1 {
		t.Fatalf("dry run: %+v", res)
	}
	res := a.SweepScratch(false, 0)
	if len(res.Removed) != 1 || filepath.Base(res.Removed[0].Path) != "TestOld" {
		t.Fatalf("sweep: %+v", res)
	}
	es, err := a.Store.Events(50)
	must(t, err)
	found := false
	for _, e := range es {
		found = found || e.Kind == EventScratchSwept
	}
	if !found {
		t.Fatal("no scratch_swept event")
	}
}
