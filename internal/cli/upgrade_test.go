package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/release"
	"github.com/brandonapol/saddle/internal/store"
)

type memSource struct {
	rel   release.Release
	files map[string][]byte
}

func (m memSource) Latest(context.Context) (release.Release, error) { return m.rel, nil }
func (m memSource) Download(_ context.Context, url string) ([]byte, error) {
	if b, ok := m.files[url]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("404 %s", url)
}

// upgradeEnv fakes a v0.2.0 release whose binary is a shell script, a
// saddle repo with state.db, and an installed v0.1.0 binary to replace.
func upgradeEnv(t *testing.T, body string) (root, exe string, src *memSource) {
	t.Helper()
	root = t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".saddle"), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(root, ".saddle", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	t.Setenv("SADDLE_ROOT", root)

	exe = filepath.Join(t.TempDir(), "saddle")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho old\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldExe := selfExe
	selfExe = func() (string, error) { return exe, nil }
	t.Cleanup(func() { selfExe = oldExe })
	oldV := Version
	Version = "v0.1.0"
	t.Cleanup(func() { Version = oldV })

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "saddle", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()
	name := release.ArchiveName("v0.2.0", runtime.GOOS, runtime.GOARCH)
	h := sha256.Sum256(buf.Bytes())
	src = &memSource{
		rel: release.Release{Tag: "v0.2.0", Body: "### Features\n\n- saddle upgrade (#326)\n", Assets: []release.Asset{
			{Name: name, URL: "mem/" + name}, {Name: release.ChecksumsName, URL: "mem/sums"},
		}},
		files: map[string][]byte{
			"mem/" + name: buf.Bytes(),
			"mem/sums":    []byte(hex.EncodeToString(h[:]) + "  " + name + "\n"),
		},
	}
	oldSrc := releaseSource
	releaseSource = func() release.Source { return src }
	t.Cleanup(func() { releaseSource = oldSrc })
	return root, exe, src
}

func addWorker(t *testing.T, root, id, status string) {
	t.Helper()
	s, err := store.Open(filepath.Join(root, ".saddle", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.CreateTask(store.Task{ID: id, Title: id, Role: store.RoleWorker, Status: status}); err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeCmdRefusesWithRunningAgents(t *testing.T) {
	// The new binary records that it ran migrate.
	root, exe, _ := upgradeEnv(t, "#!/bin/sh\necho \"$@\" > \"$SADDLE_ROOT/migrated\"\n")
	addWorker(t, root, "t5", store.Running)
	addWorker(t, root, "t6", store.Done)

	out, _, err := runRoot(t, "upgrade")
	if !errors.Is(err, release.ErrAgentsRunning) || !strings.Contains(err.Error(), "t5") || strings.Contains(err.Error(), "t6") {
		t.Fatalf("err = %v, want refusal naming t5 only", err)
	}
	if !strings.Contains(out, "- saddle upgrade (#326)") {
		t.Errorf("changelog not shown: %q", out)
	}
	if b, _ := os.ReadFile(exe); !strings.Contains(string(b), "echo old") {
		t.Fatal("binary replaced while an agent runs")
	}

	out, _, err = runRoot(t, "upgrade", "--force")
	if err != nil {
		t.Fatalf("upgrade --force: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(exe); strings.Contains(string(b), "echo old") {
		t.Fatal("--force did not replace the binary")
	}
	if b, _ := os.ReadFile(filepath.Join(root, "migrated")); strings.TrimSpace(string(b)) != "migrate" {
		t.Fatalf("new binary ran with %q, want migrate", b)
	}
	if bs, _ := filepath.Glob(filepath.Join(root, ".saddle", "state.db.bak-v0.1.0-*")); len(bs) != 1 {
		t.Fatalf("state.db backups = %v, want one before migrating", bs)
	} else if v, err := store.FileSchemaVersion(bs[0]); err != nil || v != store.SchemaVersion() {
		t.Fatalf("backup is not a database: %d, %v", v, err)
	}
	if !strings.Contains(out, "Upgraded saddle v0.1.0 -> v0.2.0") {
		t.Errorf("out = %q", out)
	}
}

func TestUpgradeCmdRejectsTamperedArchive(t *testing.T) {
	root, exe, src := upgradeEnv(t, "#!/bin/sh\necho new\n")
	name := release.ArchiveName("v0.2.0", runtime.GOOS, runtime.GOARCH)
	src.files["mem/"+name] = append([]byte{}, append(src.files["mem/"+name], 0)...)
	_, _, err := runRoot(t, "upgrade")
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v, want checksum mismatch", err)
	}
	if b, _ := os.ReadFile(exe); !strings.Contains(string(b), "echo old") {
		t.Fatal("tampered binary installed")
	}
	if bs, _ := filepath.Glob(filepath.Join(root, ".saddle", "*.bak-*")); len(bs) != 0 {
		t.Fatalf("backups = %v, want none for a refused upgrade", bs)
	}
}

func TestUpgradeCmdCheck(t *testing.T) {
	_, exe, _ := upgradeEnv(t, "#!/bin/sh\necho new\n")
	out, _, err := runRoot(t, "upgrade", "--check")
	if err != nil || !strings.Contains(out, "v0.1.0 -> v0.2.0") {
		t.Fatalf("upgrade --check = %q, %v", out, err)
	}
	if b, _ := os.ReadFile(exe); !strings.Contains(string(b), "echo old") {
		t.Fatal("--check replaced the binary")
	}
}

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
