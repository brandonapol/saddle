package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
)

// storeApp is an App over a fresh store in a temp dir: enough for commands
// that only read the store and .saddle/ files.
func storeApp(t *testing.T) *app.App {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, ".saddle", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Default()
	cfg.Serial = []string{"go.mod"}
	return &app.App{Root: root, Cfg: cfg, Store: st}
}

func addTask(t *testing.T, a *app.App, tk store.Task, claims ...string) {
	t.Helper()
	if tk.Role == "" {
		tk.Role = store.RoleWorker
	}
	if err := a.Store.CreateTask(tk); err != nil {
		t.Fatal(err)
	}
	if len(claims) > 0 {
		if _, err := a.Store.Claim(tk.ID, func(map[string][]string) ([]string, error) { return claims, nil }); err != nil {
			t.Fatal(err)
		}
	}
}

// #37: saddle brief shows a task's goal, its claims, other tasks' claims as
// hands-off, the done-when list from its prompt and its children.
func TestBriefShowsClaimsAndChildren(t *testing.T) {
	a := storeApp(t)
	addTask(t, a, store.Task{ID: "t1", Title: "usage meter", Status: store.Running, Parent: "t0",
		Prompt: "Build the usage meter. It must not call GitHub.\n\nDone when:\n- [ ] meter tests pass\n- [x] wired into the header\n"},
		"internal/usage/**")
	addTask(t, a, store.Task{ID: "t2", Title: "meter cli", Status: store.Done, Parent: "t1"}, "internal/cli/meter.go")
	addTask(t, a, store.Task{ID: "t3", Title: "unrelated", Status: store.Running, Parent: "t0"}, "internal/app/**")

	var out strings.Builder
	if err := writeTaskBrief(&out, a, "t1", 80); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"t1 usage meter", "running · from t0",
		"Goal\n  Build the usage meter.",
		"Owns\n  internal/usage/**",
		"Hands off\n  go.mod\n  internal/app/** (t3)\n  internal/cli/meter.go (t2)",
		"Done when\n  [ ] meter tests pass\n  [x] wired into the header",
		"Children\n  t2 done meter cli",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("brief lacks %q:\n%s", want, got)
		}
	}
	if err := writeTaskBrief(&out, a, "t9", 80); err == nil {
		t.Error("an unknown task should be an error")
	}
}

func TestBriefCommandShape(t *testing.T) {
	c := briefCmd()
	if c.Name() != "brief" || c.Flags().Lookup("watch") == nil || c.Flags().Lookup("width") == nil {
		t.Fatalf("brief command: %s", c.UsageString())
	}
}

func TestBriefAndTmuxRegistered(t *testing.T) {
	if c, _, err := Root().Find([]string{"brief"}); err != nil || c.Name() != "brief" {
		t.Fatalf("saddle brief not registered: %v", err)
	}
	if c, _, err := Root().Find([]string{"status"}); err != nil || c.Flags().Lookup("tmux") == nil {
		t.Fatalf("saddle status --tmux not registered: %v", err)
	}
}
