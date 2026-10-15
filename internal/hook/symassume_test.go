package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
)

// #592: PostToolUse returns the stale-assumption note in additionalContext
// together with notices already queued for the task.
func TestPostToolUseAssumeNoteAlongsideNotices(t *testing.T) {
	a := setup(t)
	a.Cfg.Claims.Granularity = config.GranularitySymbol
	storeSrc := "package store\n\nfunc (s *Store) Claim(path string) error {\n\treturn nil\n}\n"
	useSrc := "package store\n\nfunc Use(s *Store) error {\n\treturn nil\n}\n"
	if err := os.MkdirAll(filepath.Join(a.Root, "store"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.Root, "store", "store.go"), []byte(storeSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.Root, "store", "use.go"), []byte(useSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOK(t, a.Root, "add", "-A")
	gitOK(t, a.Root, "commit", "-qm", "add store")

	t1, err := a.Spawn(app.SpawnReq{Title: "change claim"})
	if err != nil {
		t.Fatal(err)
	}
	t2, err := a.Spawn(app.SpawnReq{Title: "use claim"})
	if err != nil {
		t.Fatal(err)
	}
	changed := "package store\n\nfunc (s *Store) Claim(path string, force bool) error {\n\treturn nil\n}\n"
	if err := os.WriteFile(filepath.Join(t1.Worktree, "store", "store.go"), []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	after := "package store\n\nfunc Use(s *Store) error {\n\treturn s.Claim(\"a\")\n}\n"
	usePath := filepath.Join(t2.Worktree, "store", "use.go")
	if err := os.WriteFile(usePath, []byte(after), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.Notify(t2.ID, store.NoticeInfo, "queued notice"); err != nil {
		t.Fatal(err)
	}

	out := run(t, a, t2.ID, map[string]any{
		"hook_event_name": "PostToolUse", "tool_name": "Edit",
		"tool_input":    map[string]any{"file_path": usePath},
		"tool_response": map[string]any{"originalFile": useSrc},
	})
	hs, _ := out["hookSpecificOutput"].(map[string]any)
	ctx, _ := hs["additionalContext"].(string)
	for _, want := range []string{
		"[saddle] queued notice",
		t1.ID,
		"`Store.Claim`",
		"store/store.go",
		"func (s *Store) Claim(path string) error",
		"func (s *Store) Claim(path string, force bool) error",
	} {
		if !strings.Contains(ctx, want) {
			t.Errorf("additionalContext %q lacks %q", ctx, want)
		}
	}
}
