package app

import (
	"path/filepath"
	"testing"
)

func fetchFailed(t *testing.T, a *App) bool {
	t.Helper()
	es, err := a.Store.Events(100)
	must(t, err)
	for _, e := range es {
		if e.Kind == "fetch_failed" {
			return true
		}
	}
	return false
}

// TestOrchestratorLeavesTheBaseFetchForLater (#635): starting the
// orchestrator fetched base from the remote every saddle up, about a second
// before the first frame, only to warn when integration was behind. With
// integration in place it no longer fetches; CheckIntegrationBehind, which
// saddle up runs after its first frame, still does.
func TestOrchestratorLeavesTheBaseFetchForLater(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	must(t, a.Init())
	must(t, a.ensureIntegration())
	// A remote that can't be fetched: every fetch records fetch_failed.
	git(t, a.Root, "remote", "add", "origin", filepath.Join(t.TempDir(), "missing.git"))
	if _, _, err := a.Orchestrator(); err != nil {
		t.Fatal(err)
	}
	if fetchFailed(t, a) {
		t.Fatal("Orchestrator fetched base")
	}
	a.CheckIntegrationBehind()
	if !fetchFailed(t, a) {
		t.Fatal("CheckIntegrationBehind didn't fetch base")
	}
}
