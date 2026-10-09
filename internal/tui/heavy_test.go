package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/runq"
)

// realHeavy builds a private heavy-run queue in enforce mode with one
// go-test slot (max_run 1ms, so the holder is overdue): t83 of this repo
// holds it, t84 from repo "other" and an untasked "pid 4242" wait in that
// order. It returns the queue as the app reads it.
func realHeavy(t *testing.T) app.HeavyRuns {
	t.Helper()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "runq.db")
	t.Setenv(runq.EnvPath, db)
	t.Setenv(runq.EnvBypass, "")
	t.Setenv(runq.EnvLease, "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
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
	gone := make(chan struct{}, 2)
	t.Cleanup(func() { cancel(); <-gone; <-gone; _ = q.Close() })
	h, err := q.Acquire(ctx, runq.Request{Class: "go-test", Label: "t83", Repo: filepath.Base(root), Cmd: "make check"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Release() })
	for i, w := range []struct{ label, repo string }{{"t84", "other"}, {"pid 4242", filepath.Base(root)}} {
		go func() {
			defer func() { gone <- struct{}{} }()
			_, _ = q.Acquire(ctx, runq.Request{Class: "go-test", Label: w.label, Repo: w.repo, Cmd: "go test ./..."})
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
				t.Fatalf("%s never queued", w.label)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	v, err := (&app.App{Root: root}).HeavyRuns()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// heavySnap is a queue with an overdue holder and two waiters, without a
// real queue behind it.
func heavySnap() app.HeavyRuns {
	return app.HeavyRuns{Mode: "enforce", Repo: "saddle", Classes: []app.HeavyClass{
		{Class: "e2e", Slots: 1, MaxRunMS: 1_800_000, Holders: []app.HeavyEntry{}, Waiters: []app.HeavyEntry{}},
		{Class: "go-test", Slots: 1, MaxRunMS: 1_800_000,
			Holders: []app.HeavyEntry{{Lease: "aaaa1111bbbb", Task: "t83", Repo: "saddle", Label: "t83", Cmd: "make check", AgeMS: 40 * 60_000, Overdue: true}},
			Waiters: []app.HeavyEntry{
				{Lease: "cccc2222dddd", Task: "t84", Repo: "other", Label: "t84", Cmd: "go test ./...", Position: 1, AgeMS: 6 * 60_000, ETAMS: 2 * 60_000},
				{Lease: "eeee3333ffff", Label: "pid 4242", Repo: "saddle", Cmd: "go test ./...", Position: 2, AgeMS: 30_000},
			}},
	}}
}

type fakeKill struct{ leases []string }

func (f *fakeKill) kill(lease string) error {
	f.leases = append(f.leases, lease)
	return nil
}

func newRunsModel(t *testing.T, w, h int, v app.HeavyRuns) (*model, *fakeKill) {
	t.Helper()
	m := newViewModel(w, h)
	f := &fakeKill{}
	m.hv.kill = f.kill
	m.hv.read = func() (app.HeavyRuns, error) { return v, nil }
	m.Update(heavyMsg{v: v})
	return m, f
}

// TestRunsViewShowsQueuePositions: with one holder and two waiters read
// from a real queue, the header's segment names the class, holder and
// waiters, and the runs view (alt+4) lists them at the same positions
// saddle status and the MCP status tool show (#242).
func TestRunsViewShowsQueuePositions(t *testing.T) {
	v := realHeavy(t)
	m, _ := newRunsModel(t, 200, 30, v)
	if h := m.viewHeader(); !strings.Contains(h, "go-test ▸t83 ") || !strings.Contains(h, "· 2 waiting") {
		t.Fatalf("header lacks the heavy-run segment:\n%s", h)
	}
	press(m, altKey('4'))
	if m.view != viewRuns {
		t.Fatalf("alt+4 left the view at %d", m.view)
	}
	screen := flat(m.View())
	for _, want := range []string{"HEAVY RUNS", "go-test", "1/1 slots busy", "t83", "make check", "overdue", "#1 other/t84", "#2 pid 4242"} {
		if !strings.Contains(screen, want) {
			t.Errorf("runs view lacks %q:\n%s", want, screen)
		}
	}
	if strings.Index(screen, "#1 other/t84") > strings.Index(screen, "#2 pid 4242") {
		t.Errorf("waiters out of order:\n%s", screen)
	}
	for _, w := range []int{36, 80, 140} {
		m.Update(tea.WindowSizeMsg{Width: w, Height: 30})
		checkScreen(t, "runs", m.View(), w, 30)
	}
}

// TestRunsViewKillAsksFirst: x on a lease asks before killing it; any key
// but y keeps it, y kills that lease and reloads the queue.
func TestRunsViewKillAsksFirst(t *testing.T) {
	m, f := newRunsModel(t, 120, 30, heavySnap())
	press(m, altKey('4'))
	press(m, runeKey('j')) // the first waiter
	press(m, runeKey('x'))
	if len(f.leases) != 0 {
		t.Fatalf("x killed without asking: %v", f.leases)
	}
	if foot := flat(m.viewFooter()); !strings.Contains(foot, "Kill lease cccc2222 (other/t84, go test ./...)? y kills it") {
		t.Fatalf("footer while asking: %q", foot)
	}
	press(m, runeKey('n'))
	if len(f.leases) != 0 || m.hv.confirm != "" {
		t.Fatalf("n should keep the lease: killed %v, confirm %q", f.leases, m.hv.confirm)
	}
	press(m, runeKey('x'))
	press(m, runeKey('y'))
	if len(f.leases) != 1 || f.leases[0] != "cccc2222dddd" {
		t.Fatalf("killed %v, want the first waiter's lease", f.leases)
	}
	if !strings.Contains(m.flash, "killed lease cccc2222") {
		t.Fatalf("flash %q", m.flash)
	}
}

// TestHeavySegmentQuietWhenIdle: an idle queue adds nothing to the header,
// and an overdue holder is marked.
func TestHeavySegmentQuietWhenIdle(t *testing.T) {
	m, _ := newRunsModel(t, 200, 30, app.HeavyRuns{Mode: "enforce", Classes: []app.HeavyClass{{Class: "go-test", Slots: 2}}})
	if h := m.viewHeader(); strings.Contains(h, "go-test") {
		t.Fatalf("idle queue in header:\n%s", h)
	}
	m.Update(heavyMsg{v: heavySnap()})
	if h := m.viewHeader(); !strings.Contains(h, "go-test ▸t83 40m! · 2 waiting") || strings.Contains(h, "e2e") {
		t.Fatalf("header %q", h)
	}
}
