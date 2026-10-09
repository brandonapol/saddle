package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/runq"
)

// TestStatusShowsHeavyRuns: with one holder and two waiters, the status
// tool's heavy_runs lists the class's slots, the holder (overdue past
// max_run) and the waiters at the queue's positions, each mapped to its
// task and repo (#242).
func TestStatusShowsHeavyRuns(t *testing.T) {
	a, _ := stackSetup(t)
	db := filepath.Join(t.TempDir(), "runq.db")
	t.Setenv(runq.EnvPath, db)
	t.Setenv(runq.EnvBypass, "")
	t.Setenv(runq.EnvLease, "")
	toml := "mode = \"enforce\"\nmax_load_per_cpu = 0\nmax_cpu_pressure = 0\nheartbeat = \"200ms\"\n[classes.go-test]\nslots = 1\nmax_run = \"1ms\"\n"
	if err := os.WriteFile(app.RunqConfigPath(a.Root), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := callStatus(t, a); out.HeavyRuns != nil {
		t.Fatalf("idle queue in status: %+v", out.HeavyRuns)
	}
	q, err := runq.Open(runq.Options{Path: db, Mode: runq.ModeEnforce, Slots: map[string]int{}, Heartbeat: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	gone := make(chan struct{}, 2)
	t.Cleanup(func() { cancel(); <-gone; <-gone; _ = q.Close() })
	repo := filepath.Base(a.Root)
	h, err := q.Acquire(ctx, runq.Request{Class: "go-test", Label: "t83", Repo: repo, Cmd: "make check"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Release() })
	for i, label := range []string{"t84", "t85"} {
		go func() {
			defer func() { gone <- struct{}{} }()
			_, _ = q.Acquire(ctx, runq.Request{Class: "go-test", Label: label, Repo: repo, Cmd: "go test ./..."})
		}()
		deadline := time.Now().Add(10 * time.Second)
		for {
			st, err := q.Status()
			if err != nil {
				t.Fatal(err)
			}
			if len(st) > 0 && len(st[0].Waiters) == i+1 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s never queued", label)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	out := callStatus(t, a)
	if out.HeavyRuns == nil || out.HeavyRuns.Mode != "enforce" || len(out.HeavyRuns.Classes) != 1 {
		t.Fatalf("heavy_runs %+v", out.HeavyRuns)
	}
	c := out.HeavyRuns.Classes[0]
	if c.Class != "go-test" || c.Slots != 1 || len(c.Holders) != 1 || c.Holders[0].Task != "t83" || c.Holders[0].Repo != repo ||
		!c.Holders[0].Overdue || len(c.Waiters) != 2 || c.Waiters[0].Task != "t84" || c.Waiters[0].Position != 1 ||
		c.Waiters[1].Task != "t85" || c.Waiters[1].Position != 2 {
		t.Fatalf("go-test %+v", c)
	}
}
