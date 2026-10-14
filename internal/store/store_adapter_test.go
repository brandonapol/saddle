package store

import "testing"

// A task row keeps its adapter, and a row written before the column existed
// reads back with a blank one (#507).
func TestTaskAdapter(t *testing.T) {
	s, err := Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.CreateTask(Task{ID: "t1", Title: "g", Status: Running, Adapter: "grok"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO tasks(id, title, status, created_at, updated_at) VALUES('t2', 'old', 'running', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Task("t1"); got.Adapter != "grok" {
		t.Fatalf("adapter %q", got.Adapter)
	}
	if got, _ := s.Task("t2"); got.Adapter != "" {
		t.Fatalf("old row adapter %q", got.Adapter)
	}
	if err := s.SetField("t1", "adapter", "codex"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Task("t1"); got.Adapter != "codex" {
		t.Fatalf("adapter after SetField %q", got.Adapter)
	}
}
