package cli

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/store"
)

func runAfter(t *testing.T, a *app.App, args ...string) (string, error) {
	t.Helper()
	c := afterCmd()
	t.Chdir(a.Root)
	t.Setenv("SADDLE_ROOT", a.Root)
	if err := os.WriteFile(a.Root+"/.saddle/config.toml", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	c.SetOut(&out)
	c.SetArgs(args)
	err := c.Execute()
	return out.String(), err
}

// #675: saddle after <task> <dep>... records after edges post-spawn.
func TestAfterCommand(t *testing.T) {
	a := storeApp(t)
	addTask(t, a, store.Task{ID: "t1", Title: "api", Status: store.Landed})
	addTask(t, a, store.Task{ID: "t2", Title: "use", Status: store.Landed})
	out, err := runAfter(t, a, "t2", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "t2 now runs after t1") {
		t.Fatalf("output = %q", out)
	}
	if got := a.TaskAfter("t2"); !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("after = %v, want [t1]", got)
	}
	if _, err := runAfter(t, a, "t1", "t2"); err == nil {
		t.Fatal("recorded a cycle")
	}
	if _, err := runAfter(t, a, "t2"); err == nil {
		t.Fatal("ran with no dependency")
	}
}
