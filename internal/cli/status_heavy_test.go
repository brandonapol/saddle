package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/runq"
	"github.com/brandonapol/saddle/internal/store"
)

// heavyQueue gives root a private heavy-run queue in enforce mode with one
// go-test slot (max_run 1ms, so the holder is overdue), held by t83 of this
// repo, with t84 from repo "other" and an untasked "pid 4242" waiting
// behind it in that order.
func heavyQueue(t *testing.T, root string) {
	t.Helper()
	db := filepath.Join(t.TempDir(), "runq.db")
	t.Setenv(runq.EnvPath, db)
	t.Setenv(runq.EnvBypass, "")
	t.Setenv(runq.EnvLease, "")
	if err := os.MkdirAll(filepath.Join(root, ".saddle"), 0o755); err != nil {
		t.Fatal(err)
	}
	toml := "mode = \"enforce\"\nmax_load_per_cpu = 0\nmax_cpu_pressure = 0\nheartbeat = \"200ms\"\n[classes.go-test]\nslots = 1\nmax_run = \"1ms\"\n"
	if err := os.WriteFile(app.RunqConfigPath(root), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	q, err := runq.Open(runq.Options{Path: db, Mode: runq.ModeEnforce, Slots: map[string]int{}, Heartbeat: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var gone []chan struct{}
	t.Cleanup(func() {
		cancel()
		for _, g := range gone {
			<-g
		}
		_ = q.Close()
	})
	h, err := q.Acquire(ctx, runq.Request{Class: "go-test", Label: "t83", Repo: runq.RepoLabel(root), Cmd: "make check"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Release() })
	for i, w := range []struct{ label, repo string }{{"t84", "other"}, {"pid 4242", runq.RepoLabel(root)}} {
		g := make(chan struct{})
		gone = append(gone, g)
		go func() {
			defer close(g)
			_, _ = q.Acquire(ctx, runq.Request{Class: "go-test", Prio: runq.PrioWorker, Label: w.label, Repo: w.repo, Cmd: "go test ./..."})
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
				t.Fatalf("waiter %s never queued", w.label)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func runStatus(t *testing.T, a *app.App, args ...string) string {
	t.Helper()
	c := withTmux(statusCmd())
	t.Chdir(a.Root)
	t.Setenv("SADDLE_ROOT", a.Root)
	if err := os.WriteFile(a.Root+"/.saddle/config.toml", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	c.SetOut(&out)
	c.SetArgs(args)
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// TestStatusShowsHeavyRuns: with one holder and two waiters, saddle status
// has a Heavy runs section naming the class's slots, the overdue holder
// and each waiter's position, and --json carries the same (#242).
func TestStatusShowsHeavyRuns(t *testing.T) {
	a := storeApp(t)
	addTask(t, a, store.Task{ID: "t83", Title: "tests", Status: store.Running})
	heavyQueue(t, a.Root)

	out := runStatus(t, a)
	_, sect, ok := strings.Cut(out, "Heavy runs (enforce)\n")
	if !ok {
		t.Fatalf("no Heavy runs section:\n%s", out)
	}
	lines := strings.Split(strings.TrimRight(sect, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("heavy runs section:\n%s", sect)
	}
	for i, want := range [][]string{
		{"go-test", "1/1 slots busy", "2 waiting"},
		{"▸ t83", "make check", "overdue (max_run ", "lease "},
		{"#1 other/t84", "go test ./...", "waiting "},
		{"#2 pid 4242", "go test ./...", "waiting "},
	} {
		for _, w := range want {
			if !strings.Contains(lines[i], w) {
				t.Errorf("line %d %q lacks %q", i, lines[i], w)
			}
		}
	}

	var js struct {
		HeavyRuns *app.HeavyRuns `json:"heavy_runs"`
	}
	if err := json.Unmarshal([]byte(runStatus(t, a, "--json")), &js); err != nil {
		t.Fatal(err)
	}
	if js.HeavyRuns == nil || len(js.HeavyRuns.Classes) != 1 {
		t.Fatalf("json heavy_runs %+v", js.HeavyRuns)
	}
	c := js.HeavyRuns.Classes[0]
	if len(c.Holders) != 1 || c.Holders[0].Task != "t83" || !c.Holders[0].Overdue || len(c.Waiters) != 2 ||
		c.Waiters[0].Position != 1 || c.Waiters[0].Task != "t84" || c.Waiters[0].Repo != "other" ||
		c.Waiters[1].Position != 2 || c.Waiters[1].Label != "pid 4242" {
		t.Fatalf("json class %+v", c)
	}
}

// TestStatusHeavyRunsQuietWhenIdle: an idle queue adds no section.
func TestStatusHeavyRunsQuietWhenIdle(t *testing.T) {
	a := storeApp(t)
	t.Setenv(runq.EnvPath, filepath.Join(t.TempDir(), "runq.db"))
	if out := runStatus(t, a); strings.Contains(out, "Heavy runs") {
		t.Fatalf("idle queue printed a section:\n%s", out)
	}
}
