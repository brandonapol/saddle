package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

func TestMigrateCmd(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SADDLE_ROOT", root)
	out, _, err := runRoot(t, "migrate")
	if err != nil || !strings.Contains(out, "No state.db") {
		t.Fatalf("migrate without db = %q, %v", out, err)
	}
	s, err := store.Open(filepath.Join(root, ".saddle", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	out, _, err = runRoot(t, "migrate")
	if err != nil || !strings.Contains(out, "nothing to migrate") {
		t.Fatalf("migrate on a current db = %q, %v", out, err)
	}
}
