package app

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

// #176: a runtime override beats the config's concurrency, is logged, and
// survives a restart; reset goes back to the config.
func TestConcurrencyOverrideBeatsConfigAndPersists(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	a.Cfg.Concurrency = 5
	c, err := a.Concurrency()
	must(t, err)
	if c.Limit != 5 || c.Source != ConcurrencyFromConfig || c.Config != 5 {
		t.Fatalf("unset: %+v, want 5 from config", c)
	}
	c, err = a.SetConcurrency(2)
	must(t, err)
	if c.Limit != 2 || c.Source != ConcurrencyRuntime || a.ConcurrencyLimit() != 2 {
		t.Fatalf("set 2: %+v", c)
	}
	es, err := a.Store.Events(50)
	must(t, err)
	var logged []string
	for _, e := range es {
		if e.Kind == EventConcurrency {
			logged = append(logged, e.Data)
		}
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "5 → 2") {
		t.Fatalf("concurrency events = %q, want one 5 → 2", logged)
	}

	b, err := Open(a.Root)
	must(t, err)
	defer b.Close()
	b.Cfg.Concurrency = 5
	if got := b.ConcurrencyLimit(); got != 2 {
		t.Fatalf("after restart limit = %d, want the override 2", got)
	}

	c, err = a.ResetConcurrency()
	must(t, err)
	if c.Limit != 5 || c.Source != ConcurrencyFromConfig {
		t.Fatalf("reset: %+v, want 5 from config", c)
	}
}

func TestSetConcurrencyRange(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	for _, n := range []int{0, -1, MaxConcurrency + 1} {
		if _, err := a.SetConcurrency(n); err == nil {
			t.Errorf("SetConcurrency(%d) succeeded", n)
		}
	}
	for _, n := range []int{MinConcurrency, MaxConcurrency} {
		if _, err := a.SetConcurrency(n); err != nil {
			t.Errorf("SetConcurrency(%d): %v", n, err)
		}
	}
}

// #176: lowering the cap kills nothing; spawn refuses with a clear message
// until enough tasks finish, then works again.
func TestSpawnHonorsLoweredConcurrency(t *testing.T) {
	t.Parallel()
	a, ft := setup(t)
	a.Cfg.Concurrency = 5
	t1, err := a.Spawn(SpawnReq{Title: "one"})
	must(t, err)
	t2, err := a.Spawn(SpawnReq{Title: "two"})
	must(t, err)
	_, err = a.SetConcurrency(1)
	must(t, err)
	_, err = a.Spawn(SpawnReq{Title: "three"})
	if err == nil {
		t.Fatal("spawned over the lowered cap")
	}
	for _, want := range []string{"concurrency cap", "2 running", "limit 1", "saddle concurrency", "nothing was stopped"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("spawn error lacks %q: %v", want, err)
		}
	}
	for _, id := range []string{t1.ID, t2.ID} {
		tk, err := a.Store.Task(id)
		must(t, err)
		if tk.Status != store.Running || !ft.windows[tk.Window] {
			t.Fatalf("%s is %s (window alive %v) after lowering the cap", id, tk.Status, ft.windows[tk.Window])
		}
	}
	must(t, a.Store.SetStatus(t1.ID, store.Done))
	if _, err := a.Spawn(SpawnReq{Title: "three"}); err == nil {
		t.Fatal("one running is at a cap of 1, yet spawn worked")
	}
	must(t, a.Store.SetStatus(t2.ID, store.Done))
	if _, err := a.Spawn(SpawnReq{Title: "three"}); err != nil {
		t.Fatalf("under the cap again: %v", err)
	}
}
