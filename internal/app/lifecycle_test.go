package app

import (
	"path/filepath"
	"strings"
	"testing"
)

// withOrigin gives a's repo a bare origin and returns a second clone that can
// push commits local main doesn't have.
func withOrigin(t *testing.T, a *App) (other string) {
	t.Helper()
	origin := filepath.Join(t.TempDir(), "origin.git")
	git(t, a.Root, "init", "-q", "--bare", "-b", "main", origin)
	git(t, a.Root, "remote", "add", "origin", origin)
	git(t, a.Root, "push", "-q", "-u", "origin", "main")
	other = filepath.Join(t.TempDir(), "other")
	git(t, a.Root, "clone", "-q", origin, other)
	return other
}

func pushCommit(t *testing.T, dir, file string) {
	t.Helper()
	write(t, dir, file, file+"\n")
	commitAll(t, dir, "add "+file)
	git(t, dir, "push", "-q", "origin", "main")
}

func hasEvent(t *testing.T, a *App, kind, sub string) bool {
	t.Helper()
	es, err := a.Store.Events(500)
	must(t, err)
	for _, e := range es {
		if e.Kind == kind && strings.Contains(e.Data, sub) {
			return true
		}
	}
	return false
}

// #84: integration is cut from origin/<base>, not a stale local base, and
// falling behind origin is reported once.
func TestIntegrationCutFromOrigin(t *testing.T) {
	a, _ := setup(t)
	other := withOrigin(t, a)
	pushCommit(t, other, "upstream1.txt")

	if _, err := a.Spawn(SpawnReq{Title: "one"}); err != nil {
		t.Fatal(err)
	}
	tip := git(t, other, "rev-parse", "HEAD")
	if mb := git(t, a.Root, "merge-base", a.Cfg.Integration, tip); mb != tip {
		t.Fatalf("integration merge-base = %s, want origin/main %s", mb, tip)
	}

	pushCommit(t, other, "upstream2.txt")
	if _, err := a.Spawn(SpawnReq{Title: "two"}); err != nil {
		t.Fatal(err)
	}
	if !hasEvent(t, a, "integration_behind", "integration behind origin/main by 1") {
		t.Fatal("no integration_behind event")
	}
	if _, err := a.Spawn(SpawnReq{Title: "three"}); err != nil {
		t.Fatal(err)
	}
	es, _ := a.Store.Events(500)
	n := 0
	for _, e := range es {
		if e.Kind == "integration_behind" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("integration_behind events = %d, want 1", n)
	}
	if ws := a.Warnings(); len(ws) != 1 || ws[0] != "integration behind origin/main by 1" {
		t.Fatalf("warnings = %q", ws)
	}
}
