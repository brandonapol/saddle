//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	harness "github.com/brandonapol/saddle/internal/e2e"
	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
)

// age writes an n-byte file in dir and backdates both by age.
func age(t *testing.T, dir string, n int, by time.Duration) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "f")
	if n >= 0 {
		if err := os.WriteFile(f, make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Now().Add(-by)
	for _, p := range []string{f, dir} {
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// TestScratchFillAndRecover (#322, #383): the real saddle binary with its
// scratch root capped like a 64K tmpfs ([tmp] max_bytes, read through the
// config package). A build fills the root: spawn sweeps harder, still finds
// it low, holds the spawn and says what uses the space, and status warns.
// The build dies and its dir goes stale: the next spawn's own sweep removes
// it and the spawn goes ahead, with no human step. The owner's own files in
// the OS temp dir are never touched.
func TestScratchFillAndRecover(t *testing.T) {
	if harness.Fork(t) {
		return
	}
	w := harness.New(t, bins, harness.Options{})
	t.Cleanup(w.Diagnose)
	root := filepath.Join(w.Root, "scratch")
	w.WriteConfig(harness.Options{Tables: "[tmp]\ndir = \"" + root + "\"\nmax_bytes = \"64K\"\nlow_free_pct = 15\n"})
	// The World's dir is this saddle's OS temp dir (SADDLE_SCRATCH_OS_TEMP).
	mine := filepath.Join(w.Root, "swiftchain")
	age(t, mine, 70<<10, 3*time.Hour)

	build := filepath.Join(root, "go-build-running")
	age(t, build, 60<<10, 0)
	if err := (fa.Script{Steps: []fa.Step{fa.Wait("never-comes")}}).Save(w.Scripts, "t1"); err != nil {
		t.Fatal(err)
	}
	r := w.Saddle("spawn", "--id", "t1", "After the sweep", "Prompt")
	if r.Code == 0 || !strings.Contains(r.Stderr+r.Stdout, "scratch space low") || !strings.Contains(r.Stderr+r.Stdout, "go-build-running") {
		t.Fatalf("spawn with scratch full should be held, naming the build dir: %s", r)
	}
	if st := w.MustSaddle("status"); !strings.Contains(st.Stdout+st.Stderr, "scratch is low on space") {
		t.Fatalf("status shows no scratch warning while held:\n%s", st)
	}
	if !exists(build) {
		t.Fatal("the sweep removed a fresh build dir")
	}

	// The build was killed; its dir is now older than the pressure sweep's
	// 5-minute threshold and no process holds it.
	age(t, build, -1, 20*time.Minute)
	w.Spawn("t1", "After the sweep", nil, fa.Wait("never-comes"))
	if exists(build) {
		t.Fatal("the stale build dir survived the pressure sweep")
	}
	if !exists(filepath.Join(mine, "f")) {
		t.Fatal("the sweep touched a dir in the OS temp dir that isn't saddle's")
	}
	if st := w.MustSaddle("status"); strings.Contains(st.Stdout+st.Stderr, "scratch is low on space") {
		t.Fatalf("status still warns with room:\n%s", st)
	}
	if w.Task("t1").ID == "" {
		t.Fatal("t1 was not spawned")
	}
}
