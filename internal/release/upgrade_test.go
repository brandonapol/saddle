package release

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
	"strings"
	"testing"
	"time"
)

func tgz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestParseChecksums(t *testing.T) {
	got, err := ParseChecksums("aaaa  saddle_0.1.0_linux_amd64.tar.gz\nbbbb *saddle_0.1.0_darwin_arm64.tar.gz\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if got["saddle_0.1.0_linux_amd64.tar.gz"] != "aaaa" || got["saddle_0.1.0_darwin_arm64.tar.gz"] != "bbbb" {
		t.Fatalf("ParseChecksums = %v", got)
	}
	if _, err := ParseChecksums("not a checksum line"); err == nil {
		t.Fatal("ParseChecksums accepted garbage")
	}
}

func TestVerifyChecksum(t *testing.T) {
	data := []byte("the real archive")
	sums := map[string]string{"a.tar.gz": sum(data)}
	if err := VerifyChecksum(data, "a.tar.gz", sums); err != nil {
		t.Fatal(err)
	}
	if err := VerifyChecksum([]byte("tampered"), "a.tar.gz", sums); err == nil {
		t.Fatal("tampered archive verified")
	}
	if err := VerifyChecksum(data, "b.tar.gz", sums); err == nil {
		t.Fatal("archive with no checksum verified")
	}
}

func TestArchiveName(t *testing.T) {
	if got := ArchiveName("v0.1.0", "darwin", "arm64"); got != "saddle_0.1.0_darwin_arm64.tar.gz" {
		t.Fatalf("ArchiveName = %q", got)
	}
}

func TestExtractBinary(t *testing.T) {
	a := tgz(t, map[string]string{"README.md": "hi", "saddle": "#!binary"})
	got, err := ExtractBinary(a, "saddle")
	if err != nil || string(got) != "#!binary" {
		t.Fatalf("ExtractBinary = %q, %v", got, err)
	}
	if _, err := ExtractBinary(tgz(t, map[string]string{"other": "x"}), "saddle"); err == nil {
		t.Fatal("ExtractBinary found a missing binary")
	}
}

// fakeSource serves one release from memory.
type fakeSource struct {
	rel   Release
	files map[string][]byte
}

func (f fakeSource) Latest(context.Context) (Release, error) { return f.rel, nil }

func (f fakeSource) Download(_ context.Context, url string) ([]byte, error) {
	b, ok := f.files[url]
	if !ok {
		return nil, fmt.Errorf("404 %s", url)
	}
	return b, nil
}

type upgradeFixture struct {
	u       *Upgrader
	out     *bytes.Buffer
	exe     string
	db      string
	calls   *[]string
	archive []byte
	src     *fakeSource
}

func newUpgradeFixture(t *testing.T) upgradeFixture {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "bin", "saddle")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "repo", ".saddle", "state.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db, []byte("db"), 0o644); err != nil {
		t.Fatal(err)
	}
	name := ArchiveName("v0.2.0", "linux", "amd64")
	archive := tgz(t, map[string]string{"saddle": "new binary"})
	src := &fakeSource{
		rel: Release{Tag: "v0.2.0", Body: "### Features\n\n- upgrade (#1)\n\nCo-Authored-By: Claude <noreply@anthropic.com>\n", Assets: []Asset{
			{Name: name, URL: "u/" + name},
			{Name: ChecksumsName, URL: "u/" + ChecksumsName},
		}},
		files: map[string][]byte{
			"u/" + name:          archive,
			"u/" + ChecksumsName: []byte(sum(archive) + "  " + name + "\n"),
		},
	}
	var calls []string
	out := &bytes.Buffer{}
	u := &Upgrader{
		Source:  src,
		Current: Info{Version: "v0.1.0", Commit: "abc"},
		GOOS:    "linux", GOARCH: "amd64",
		Exe:     exe,
		StateDB: db,
		Running: func() ([]string, error) { return nil, nil },
		Backup: func(src, dst string) error {
			calls = append(calls, "backup "+filepath.Base(dst))
			b, err := os.ReadFile(src)
			if err != nil {
				return err
			}
			return os.WriteFile(dst, b, 0o644)
		},
		Migrate: func(newExe string) error {
			b, _ := os.ReadFile(newExe)
			calls = append(calls, "migrate with "+string(b))
			return nil
		},
		Out: out,
		Now: func() time.Time { return time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC) },
	}
	return upgradeFixture{u: u, out: out, exe: exe, db: db, calls: &calls, archive: archive, src: src}
}

func TestUpgradeReplacesBinaryAndBacksUpFirst(t *testing.T) {
	f := newUpgradeFixture(t)
	res, err := f.u.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(f.exe); string(b) != "new binary" {
		t.Fatalf("binary = %q, want the new one", b)
	}
	if b, _ := os.ReadFile(f.exe + ".prev"); string(b) != "old binary" {
		t.Fatalf("previous binary not kept for rollback: %q", b)
	}
	want := []string{"backup state.db.bak-v0.1.0-20261010T120000Z", "migrate with new binary"}
	if strings.Join(*f.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %v, want backup before migrate: %v", *f.calls, want)
	}
	if res.Backup == "" || !strings.HasSuffix(res.Backup, "state.db.bak-v0.1.0-20261010T120000Z") {
		t.Fatalf("Result.Backup = %q", res.Backup)
	}
	out := f.out.String()
	if !strings.Contains(out, "- upgrade (#1)") {
		t.Errorf("changelog not shown:\n%s", out)
	}
	if strings.Contains(out, "Co-Authored-By") {
		t.Errorf("attribution shown in changelog:\n%s", out)
	}
}

func TestUpgradeRefusesWhileAgentsRun(t *testing.T) {
	f := newUpgradeFixture(t)
	f.u.Running = func() ([]string, error) { return []string{"t3", "t4"}, nil }
	_, err := f.u.Run(context.Background())
	if !errors.Is(err, ErrAgentsRunning) || !strings.Contains(err.Error(), "t3") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("err = %v, want ErrAgentsRunning naming t3 and --force", err)
	}
	if b, _ := os.ReadFile(f.exe); string(b) != "old binary" {
		t.Fatal("binary replaced while agents run")
	}
	if len(*f.calls) != 0 {
		t.Fatalf("calls = %v, want none", *f.calls)
	}
	if !strings.Contains(f.out.String(), "- upgrade (#1)") {
		t.Error("changelog should still be shown before refusing")
	}

	f.u.Force = true
	if _, err := f.u.Run(context.Background()); err != nil {
		t.Fatalf("--force: %v", err)
	}
	if b, _ := os.ReadFile(f.exe); string(b) != "new binary" {
		t.Fatal("--force did not replace the binary")
	}
}

func TestUpgradeRejectsTamperedArchive(t *testing.T) {
	f := newUpgradeFixture(t)
	name := ArchiveName("v0.2.0", "linux", "amd64")
	f.src.files["u/"+name] = tgz(t, map[string]string{"saddle": "evil binary"})
	_, err := f.u.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err = %v, want a checksum mismatch", err)
	}
	if b, _ := os.ReadFile(f.exe); string(b) != "old binary" {
		t.Fatal("tampered binary installed")
	}
	if len(*f.calls) != 0 {
		t.Fatalf("calls = %v, want none", *f.calls)
	}
}

func TestUpgradeMissingChecksumsRefuses(t *testing.T) {
	f := newUpgradeFixture(t)
	f.src.rel.Assets = f.src.rel.Assets[:1]
	if _, err := f.u.Run(context.Background()); err == nil || !strings.Contains(err.Error(), ChecksumsName) {
		t.Fatalf("err = %v, want missing %s", err, ChecksumsName)
	}
}

func TestUpgradeAlreadyCurrent(t *testing.T) {
	f := newUpgradeFixture(t)
	f.u.Current.Version = "v0.2.0"
	res, err := f.u.Run(context.Background())
	if err != nil || res.Upgraded {
		t.Fatalf("Run = %+v, %v; want no upgrade", res, err)
	}
	if !strings.Contains(f.out.String(), "already") {
		t.Errorf("out = %q", f.out.String())
	}
}

func TestUpgradeCheckOnly(t *testing.T) {
	f := newUpgradeFixture(t)
	f.u.CheckOnly = true
	res, err := f.u.Run(context.Background())
	if err != nil || res.Upgraded {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if b, _ := os.ReadFile(f.exe); string(b) != "old binary" {
		t.Fatal("--check replaced the binary")
	}
	if !strings.Contains(f.out.String(), "v0.1.0 -> v0.2.0") {
		t.Errorf("out = %q", f.out.String())
	}
}

// Outside a saddle repo there is no state.db: nothing to back up or migrate.
func TestUpgradeWithoutStateDB(t *testing.T) {
	f := newUpgradeFixture(t)
	f.u.StateDB = ""
	res, err := f.u.Run(context.Background())
	if err != nil || res.Backup != "" {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if len(*f.calls) != 0 {
		t.Fatalf("calls = %v", *f.calls)
	}
}

func TestUpgradeMigrateFailureNamesRollback(t *testing.T) {
	f := newUpgradeFixture(t)
	f.u.Migrate = func(string) error { return errors.New("migration 5: boom") }
	_, err := f.u.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), ".prev") || !strings.Contains(err.Error(), "bak-v0.1.0") {
		t.Fatalf("err = %v, want rollback instructions", err)
	}
}
