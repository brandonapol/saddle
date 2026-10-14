package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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

func fakeBackend(sp sre.Spawner, guardRoot string) func() (sreBackend, error) {
	return func() (sreBackend, error) {
		n := 0
		next := func() (string, error) { n++; return fmt.Sprintf("t%d", n), nil }
		return sreBackend{spawner: sp, nextID: next, guardRoot: guardRoot, close: func() {}}, nil
	}
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
	err := runSRE(context.Background(), &out, in, nil, sreOpts{root: dir, runner: r, backend: fakeBackend(sp, dir)})
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

func TestSREIntakeOpensGuardIncident(t *testing.T) {
	dir := sreRepo(t, "[[check]]\nname=\"pods\"\ncommand=[\"kubectl\",\"get\",\"pods\"]\nexpect=\"Running\"\n")
	guard := t.TempDir()
	sp := &sreFakeSpawner{}
	var out bytes.Buffer
	err := runSRE(context.Background(), &out, strings.NewReader("checkout 500s"), nil,
		sreOpts{root: dir, runner: &sreFakeRunner{}, backend: fakeBackend(sp, guard)})
	if err != nil {
		t.Fatal(err)
	}
	inc, err := sre.LoadIncident(guard)
	if err != nil || !inc.Open() {
		t.Fatalf("incident = %+v, %v; want an open incident", inc, err)
	}
	if len(inc.Investigators) != len(sp.specs) {
		t.Fatalf("investigators = %q, spawned %d", inc.Investigators, len(sp.specs))
	}
	for _, s := range sp.specs {
		if !inc.Guarded("t" + string(s.Lane)) {
			t.Errorf("lane %s's task is not guarded", s.Lane)
		}
	}
	if len(inc.ReadOnly) != 1 || inc.ReadOnly[0][0] != "kubectl" {
		t.Fatalf("read-only argvs = %q, want the check commands", inc.ReadOnly)
	}

	// A second intake while the incident is open spawns nothing.
	sp2 := &sreFakeSpawner{}
	err = runSRE(context.Background(), &out, strings.NewReader("again"), nil,
		sreOpts{root: dir, runner: &sreFakeRunner{}, backend: fakeBackend(sp2, guard)})
	if err == nil || len(sp2.specs) != 0 {
		t.Fatalf("err = %v, spawned %d; want refusal while an incident is open", err, len(sp2.specs))
	}
}

type sreFailSpawner struct{}

func (sreFailSpawner) Spawn(sre.SpawnSpec) (string, error) { return "", errors.New("cap reached") }

func TestSREIntakeClosesIncidentWhenNothingSpawns(t *testing.T) {
	dir, guard := sreRepo(t, ""), t.TempDir()
	err := runSRE(context.Background(), io.Discard, strings.NewReader("db down"), nil,
		sreOpts{root: dir, runner: &sreFakeRunner{}, backend: fakeBackend(sreFailSpawner{}, guard)})
	if err == nil {
		t.Fatal("want the spawn errors")
	}
	if inc, _ := sre.LoadIncident(guard); inc.Open() {
		t.Fatal("an incident with no investigators stays open and blocks the next intake")
	}
}

// sreProbeSpawner plays an investigator whose first tool call lands before
// Spawn returns: it asks the guard, as the hook would, whether the new task
// is read-only yet.
type sreProbeSpawner struct {
	guard     string
	unguarded []string
	fail      bool
}

func (p *sreProbeSpawner) Spawn(s sre.SpawnSpec) (string, error) {
	inc, err := sre.LoadIncident(p.guard)
	if err != nil || !inc.Guarded(s.ID) {
		p.unguarded = append(p.unguarded, s.ID)
	}
	if p.fail {
		return "", errors.New("launch failed")
	}
	return s.ID, nil
}

func TestSREIntakeGuardsInvestigatorBeforeItStarts(t *testing.T) {
	dir, guard := sreRepo(t, ""), t.TempDir()
	sp := &sreProbeSpawner{guard: guard}
	err := runSRE(context.Background(), io.Discard, strings.NewReader("checkout 500s"), nil,
		sreOpts{root: dir, runner: &sreFakeRunner{}, backend: fakeBackend(sp, guard)})
	if err != nil {
		t.Fatal(err)
	}
	if len(sp.unguarded) > 0 {
		t.Fatalf("investigators %q could mutate production before they were guarded", sp.unguarded)
	}
	inc, _ := sre.LoadIncident(guard)
	if len(inc.Investigators) < 3 {
		t.Fatalf("investigators = %q", inc.Investigators)
	}
}

func TestSREIntakeDropsReservationWhenSpawnFails(t *testing.T) {
	dir, guard := sreRepo(t, ""), t.TempDir()
	sp := &sreProbeSpawner{guard: guard, fail: true}
	if err := runSRE(context.Background(), io.Discard, strings.NewReader("db down"), nil,
		sreOpts{root: dir, runner: &sreFakeRunner{}, backend: fakeBackend(sp, guard)}); err == nil {
		t.Fatal("want the spawn errors")
	}
	inc, _ := sre.LoadIncident(guard)
	if len(inc.Investigators) != 0 {
		t.Fatalf("failed spawns stay guarded: %q", inc.Investigators)
	}
}
