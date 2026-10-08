//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A World's HOME is in a temp dir. When `go` runs in it, a module cache
// under HOME is read-only, and t.TempDir's cleanup failed the journey with
// "permission denied". Go writes to the real shared caches, made writable,
// and the World makes its tree writable before it is removed.
func TestWorldCleansUpReadOnlyTrees(t *testing.T) {
	var w *World
	ok := t.Run("world", func(t *testing.T) {
		w = world(t, Options{NoInit: true})
		env := w.Env()
		if !slices.ContainsFunc(env, func(kv string) bool {
			return strings.HasPrefix(kv, "GOFLAGS=") && strings.Contains(kv, "-modcacherw")
		}) {
			t.Errorf("World env lacks GOFLAGS=-modcacherw: %q", env)
		}
		for _, k := range []string{"GOMODCACHE", "GOCACHE"} {
			i := slices.IndexFunc(env, func(kv string) bool { return strings.HasPrefix(kv, k+"=") })
			if i < 0 || strings.HasPrefix(env[i], k+"="+w.Root) {
				t.Errorf("%s is unset or inside the World: %q", k, env)
			}
		}
		// What a module download leaves: read-only files in read-only dirs.
		dir := filepath.Join(w.Home, "go", "pkg", "mod", "example.com", "m@v1.0.0", ".github")
		must(t, os.MkdirAll(dir, 0o755))
		must(t, os.WriteFile(filepath.Join(dir, "FUNDING.yml"), nil, 0o444))
		for d := dir; d != w.Home; d = filepath.Dir(d) {
			must(t, os.Chmod(d, 0o555))
		}
	})
	if !ok {
		t.Fatal("the World's cleanup failed on a read-only tree")
	}
	if _, err := os.Stat(w.Root); !os.IsNotExist(err) {
		t.Fatalf("World root %s left behind (%v)", w.Root, err)
	}
}
