package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// #801: a dead task's tmp dir holding read-only dirs (Go's module cache)
// is removed by the sweep, not left to fail every sweep after.
func TestSweepRemovesReadOnlyTaskTmp(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	dir := withScratchDir(t, a)
	tk, err := a.Spawn(SpawnReq{Title: "landed"})
	must(t, err)
	must(t, a.Store.SetStatus(tk.ID, store.Killed))
	p := wantTaskTmp(a, dir, tk.ID)
	ro := filepath.Join(p, "gomod", "pkg@v1")
	write(t, ro, "f.go", "package f\n")
	must(t, os.Chmod(ro, 0o555))
	must(t, os.Chmod(filepath.Dir(ro), 0o555))
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(ro), 0o755); _ = os.Chmod(ro, 0o755) })

	res := a.SweepScratch(false, 0)
	if len(res.Errs) > 0 {
		t.Fatalf("sweep errors: %v", res.Errs)
	}
	if dirExists(p) {
		t.Fatalf("%s survived the sweep: %+v", p, res.Kept)
	}
}

// #801: two repos share a scratch root. A sweep from A, however aggressive,
// leaves B's idle live task's TMPDIR alone: only B knows it is live.
func TestSweepLeavesOtherRepoLiveTaskTmp(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	b, _ := setup(t)
	dir := withScratchDir(t, a)
	cfg := filepath.Join(b.Root, ".saddle", "config.toml")
	old, _ := os.ReadFile(cfg)
	must(t, os.WriteFile(cfg, append(old, []byte("\n[tmp]\ndir = \""+dir+"\"\n")...), 0o644))

	tk, err := b.Spawn(SpawnReq{Title: "idle"})
	must(t, err)
	p := wantTaskTmp(b, dir, tk.ID)
	fill(t, p, 3, 3*time.Hour)

	a.SweepScratch(false, time.Nanosecond)
	if !dirExists(p) {
		t.Fatalf("a sweep from repo A removed repo B's live task dir %s", p)
	}
}
