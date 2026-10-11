package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestSchemaVersionMatchesMigrations(t *testing.T) {
	if SchemaVersion() != len(migrations) {
		t.Fatalf("SchemaVersion = %d, want %d", SchemaVersion(), len(migrations))
	}
}

// An older database is backed up before a newer binary migrates it, so an
// upgrade can be rolled back by copying the backup over state.db (#326).
func TestOpenBacksUpBeforeMigrating(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(migrations[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1; INSERT INTO tasks(id, title, status, created_at, updated_at) VALUES('t1', 'old', 'running', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	backups, _ := filepath.Glob(path + ".bak-schema1-*")
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want one state.db.bak-schema1-*", backups)
	}
	v, err := FileSchemaVersion(backups[0])
	if err != nil || v != 1 {
		t.Fatalf("backup schema = %d, %v; want 1", v, err)
	}
	if v, _ := FileSchemaVersion(path); v != SchemaVersion() {
		t.Fatalf("migrated schema = %d, want %d", v, SchemaVersion())
	}

	// Opening a current database makes no further backup.
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if again, _ := filepath.Glob(path + ".bak-*"); len(again) != 1 {
		t.Fatalf("backups after reopen = %v, want still one", again)
	}
}

func TestOpenNewDatabaseMakesNoBackup(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if b, _ := filepath.Glob(filepath.Join(dir, "*.bak-*")); len(b) != 0 {
		t.Fatalf("fresh database was backed up: %v", b)
	}
}

func TestBackupCopiesData(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	dst := filepath.Join(dir, "copy.db")
	if err := Backup(filepath.Join(dir, "state.db"), dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatal(err)
	}
	if v, _ := FileSchemaVersion(dst); v != SchemaVersion() {
		t.Fatalf("copy schema = %d", v)
	}
	if err := Backup(filepath.Join(dir, "state.db"), dst); err == nil {
		t.Fatal("Backup overwrote an existing file")
	}
}
