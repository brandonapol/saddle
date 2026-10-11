package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/doctor"
	"github.com/brandonapol/saddle/internal/release"
	"github.com/brandonapol/saddle/internal/store"
)

func runRoot(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errb bytes.Buffer
	root := Root()
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs(args)
	err = root.Execute()
	return out.String(), errb.String(), err
}

// The release build stamps version, commit and date with -ldflags, the way
// .goreleaser.yaml and the Makefile do; saddle version reports all three.
func TestVersionStampedByLdflags(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	_, file, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(file), "..", "..")
	bin := filepath.Join(t.TempDir(), "saddle")
	pkg := "github.com/brandonapol/saddle/internal/cli"
	ld := "-X " + pkg + ".Version=v9.8.7 -X " + pkg + ".Commit=0123456789abcdef -X " + pkg + ".Date=2026-10-10T12:00:00Z"
	// The Makefile's GO_TAGS, so the build cache from make test is reused.
	build := exec.Command("go", "build", "-tags", "grammar_subset grammar_subset_go grammar_subset_python", "-ldflags", ld, "-o", bin, "./cmd/saddle")
	build.Dir = repo
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	out, err := exec.Command(bin, "version").Output()
	if err != nil || strings.TrimSpace(string(out)) != "v9.8.7" {
		t.Fatalf("saddle version = %q, %v; want v9.8.7 alone", out, err)
	}
	out, err = exec.Command(bin, "version", "--json").Output()
	if err != nil {
		t.Fatal(err)
	}
	var in release.Info
	if err := json.Unmarshal(out, &in); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if in.Version != "v9.8.7" || in.Commit != "0123456789abcdef" || in.Date != "2026-10-10T12:00:00Z" || in.Schema != store.SchemaVersion() {
		t.Fatalf("version --json = %+v", in)
	}
	out, _ = exec.Command(bin, "version", "--long").Output()
	if !strings.Contains(string(out), "saddle v9.8.7 (commit 0123456789ab, built 2026-10-10T12:00:00Z") {
		t.Fatalf("version --long = %q", out)
	}
}

func TestVersionLongInProcess(t *testing.T) {
	t.Setenv("SADDLE_ROOT", "")
	out, _, err := runRoot(t, "version", "--long")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "saddle "+Version+" (commit ") || !strings.Contains(out, "state.db schema") {
		t.Fatalf("version --long = %q", out)
	}
}

// staleUp writes the up.json a running saddle up from an older binary
// leaves, and makes its pid look alive.
func staleUp(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".saddle"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := release.WriteUpRecord(filepath.Join(root, ".saddle"), release.Info{Version: "v0.0.1", Commit: "oldcommit"}, 999999, time.Now()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SADDLE_ROOT", root)
	old := processAlive
	processAlive = func(pid int) bool { return pid == 999999 }
	t.Cleanup(func() { processAlive = old })
	return root
}

func TestVersionWarnsAboutStaleUp(t *testing.T) {
	root := staleUp(t)
	oldV, oldC := Version, Commit
	Version, Commit = "v0.2.0", "newcommit"
	t.Cleanup(func() { Version, Commit = oldV, oldC })

	out, stderr, err := runRoot(t, "version")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "v0.2.0" {
		t.Fatalf("stdout = %q; the warning must not pollute it", out)
	}
	if !strings.Contains(stderr, "warning: saddle up (pid 999999) is running v0.0.1") || !strings.Contains(stderr, "v0.2.0") {
		t.Fatalf("stderr = %q, want the skew warning", stderr)
	}

	r := versionCheck(root, buildInfo())
	if r.Status != doctor.Warn || !strings.Contains(r.Detail, "v0.0.1") {
		t.Fatalf("doctor check = %+v, want a warning", r)
	}

	// Once that saddle up exits, the warning goes away.
	processAlive = func(int) bool { return false }
	if _, stderr, _ := runRoot(t, "version"); stderr != "" {
		t.Fatalf("stderr = %q after saddle up exited", stderr)
	}
	if r := versionCheck(root, buildInfo()); r.Status != doctor.OK || !strings.Contains(r.Detail, "v0.2.0") {
		t.Fatalf("doctor check = %+v", r)
	}
}

func TestRecordUpWritesAndRemoves(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".saddle"), 0o755); err != nil {
		t.Fatal(err)
	}
	remove := recordUp(root)
	rec, ok, err := release.ReadUpRecord(filepath.Join(root, ".saddle"))
	if err != nil || !ok || rec.PID != os.Getpid() || rec.Version != Version {
		t.Fatalf("up record = %+v, %v, %v", rec, ok, err)
	}
	remove()
	if _, ok, _ := release.ReadUpRecord(filepath.Join(root, ".saddle")); ok {
		t.Fatal("up record left after saddle up exits")
	}
}
