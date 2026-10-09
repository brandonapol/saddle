package runq

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestStatusRepoAndETA: Status carries each lease's repo, so cross-repo
// entries read well (#242), and each waiter's ETA: the class's median run
// time times the turns ahead of it, the same figure its wait line shows.
func TestStatusRepoAndETA(t *testing.T) {
	q := open(t, testOpts(t))
	for _, ms := range []int{60_000, 120_000, 120_000} {
		if _, err := q.db.Exec(`INSERT INTO history(class, label, cmd, waited_ms, held_ms, ended, how) VALUES('go-test','x','x',0,?,0,'ok')`, ms); err != nil {
			t.Fatal(err)
		}
	}
	h := acquire(t, q, Request{Class: "go-test", Label: "t83", Repo: "saddle", Cmd: "make check"})
	defer func() { _ = h.Release() }()
	ctx, cancel := context.WithCancel(context.Background())
	gone := make(chan struct{}, 2)
	defer func() { cancel(); <-gone; <-gone }()
	for _, label := range []string{"t84", "t85"} {
		go func() {
			defer func() { gone <- struct{}{} }()
			_, _ = q.Acquire(ctx, Request{Class: "go-test", Label: label, Repo: "other", Cmd: "go test ./..."})
		}()
		waitFor(t, label+" queued", func() bool {
			for _, w := range class(t, q, "go-test").Waiters {
				if w.Label == label {
					return true
				}
			}
			return false
		})
	}
	cs := class(t, q, "go-test")
	if len(cs.Holders) != 1 || cs.Holders[0].Repo != "saddle" || cs.Holders[0].Label != "t83" {
		t.Fatalf("holders %+v", cs.Holders)
	}
	if len(cs.Waiters) != 2 {
		t.Fatalf("waiters %+v", cs.Waiters)
	}
	for i, want := range []time.Duration{2 * time.Minute, 4 * time.Minute} {
		w := cs.Waiters[i]
		if w.Position != i+1 || w.Repo != "other" || w.ETA != want {
			t.Fatalf("waiter %d: %+v, want position %d, repo other, ETA %s", i, w, i+1, want)
		}
	}
}

// TestMaxRunConfig: max_run (default 30m) is how long a holder may run
// before status calls it overdue; a class's own max_run wins, and the
// user's file wins over the repo's.
func TestMaxRunConfig(t *testing.T) {
	if got := (Config{}).MaxRunFor("go-test"); got != 30*time.Minute {
		t.Fatalf("default max_run %s", got)
	}
	dir := t.TempDir()
	repo, user := filepath.Join(dir, "repo.toml"), filepath.Join(dir, "user.toml")
	if err := os.WriteFile(repo, []byte("max_run = \"20m\"\n[classes.e2e]\nmax_run = \"2h\"\n[classes.go-test]\nmax_run = \"5m\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(user, []byte("[classes.e2e]\nmax_run = \"1h\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(repo, user)
	if err != nil {
		t.Fatal(err)
	}
	for class, want := range map[string]time.Duration{"e2e": time.Hour, "go-test": 5 * time.Minute, "dart": 20 * time.Minute} {
		if got := c.MaxRunFor(class); got != want {
			t.Errorf("MaxRunFor(%s) = %s, want %s", class, got, want)
		}
	}
}
