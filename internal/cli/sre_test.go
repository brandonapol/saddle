package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/sre"
)

type sreFakeRunner struct{ ran [][]string }

func (f *sreFakeRunner) Run(_ context.Context, _ string, argv []string) (string, error) {
	f.ran = append(f.ran, argv)
	return "boom", errors.New("exit status 1")
}

type sreFakeSpawner struct{ specs []sre.SpawnSpec }

func (f *sreFakeSpawner) Spawn(s sre.SpawnSpec) (string, error) {
	f.specs = append(f.specs, s)
	return "t" + string(s.Lane), nil
}

func sreRepo(t *testing.T, cfg string) string {
	t.Helper()
	dir := t.TempDir()
	for _, p := range []string{".saddle", "internal/billing"} {
		if err := os.MkdirAll(filepath.Join(dir, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "internal/billing/invoice.go"), []byte("package billing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg != "" {
		if err := os.WriteFile(filepath.Join(dir, ".saddle/sre.toml"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestSREIntakeSpawnsLanes(t *testing.T) {
	dir := sreRepo(t, "[[check]]\nname=\"pods\"\ncommand=[\"kubectl\",\"get\",\"pods\"]\nexpect=\"Running\"\n")
	r, sp := &sreFakeRunner{}, &sreFakeSpawner{}
	var out bytes.Buffer
	in := strings.NewReader("500s after deploy\n/app/internal/billing/invoice.go:42\n")
	err := runSRE(context.Background(), &out, in, nil, sreOpts{
		root: dir, runner: r,
		spawner: func() (sre.Spawner, func(), error) { return sp, func() {}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.ran) != 1 || r.ran[0][0] != "kubectl" {
		t.Fatalf("preemptive checks ran %q", r.ran)
	}
	if n := len(sp.specs); n < 3 || n > 4 {
		t.Fatalf("spawned %d lanes, want 3-4\n%s", n, out.String())
	}
	for _, s := range sp.specs {
		if s.Lane == sre.LaneCode && (len(s.Claims) != 1 || s.Claims[0] != "internal/billing/invoice.go") {
			t.Fatalf("code claims = %q", s.Claims)
		}
		if s.Lane != sre.LaneCode && len(s.Claims) > 0 {
			t.Fatalf("%s claims %q", s.Lane, s.Claims)
		}
	}
	for _, want := range []string{"check pods: failed", "lane code: claims internal/billing/invoice.go", "spawned tcode for lane code"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestSREIntakeDryRunWithoutConfig(t *testing.T) {
	dir := sreRepo(t, "")
	var out bytes.Buffer
	err := runSRE(context.Background(), &out, strings.NewReader(""), []string{"postgres", "connections", "exhausted"},
		sreOpts{root: dir, dryRun: true, runner: &sreFakeRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"no .saddle/sre.toml", "lane infra", "dry run"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if err := runSRE(context.Background(), &out, strings.NewReader(" "), []string{"-"}, sreOpts{root: dir, dryRun: true}); err == nil {
		t.Fatal("empty incident: want error")
	}
}

func TestSRECommandKeepsCheckConfig(t *testing.T) {
	cmd := sreCmd()
	if c, _, err := cmd.Find([]string{"check-config"}); err != nil || c.Name() != "check-config" {
		t.Fatalf("check-config subcommand lost: %v", err)
	}
	if cmd.Flags().Lookup("dry-run") == nil {
		t.Fatal("sre has no --dry-run")
	}
}
