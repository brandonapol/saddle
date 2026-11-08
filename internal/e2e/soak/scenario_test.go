package soak

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/invariants"
)

// The same seed gives the same scenario, byte for byte, and another seed
// a different one: a failing soak is replayed from its seed.
func TestGenerateIsDeterministicBySeed(t *testing.T) {
	t.Parallel()
	for _, n := range []int{10, 50} {
		a, _ := json.Marshal(Generate(Config{Seed: 7, Changes: n}))
		b, _ := json.Marshal(Generate(Config{Seed: 7, Changes: n}))
		if string(a) != string(b) {
			t.Fatalf("%d changes: seed 7 gave two scenarios:\n%s\n%s", n, a, b)
		}
		c, _ := json.Marshal(Generate(Config{Seed: 8, Changes: n}))
		if string(a) == string(c) {
			t.Fatalf("%d changes: seeds 7 and 8 gave the same scenario", n)
		}
	}
}

// Every scenario is a DAG of after edges on earlier changes, carries every
// fault kind at least once, keeps one fault per change and stays inside
// the repair cap with its red-CI changes.
func TestGenerateShape(t *testing.T) {
	t.Parallel()
	for seed := int64(0); seed < 200; seed++ {
		for _, n := range []int{10, 50} {
			sc := Generate(Config{Seed: seed, Changes: n})
			if len(sc.Changes) != n {
				t.Fatalf("seed %d: %d changes, want %d", seed, len(sc.Changes), n)
			}
			for i, c := range sc.Changes {
				if c.ID != ChangeID(i+1) || c.Issue != i+1 {
					t.Fatalf("seed %d: change %d is %+v", seed, i, c)
				}
				if len(c.Faults) > 1 {
					t.Fatalf("seed %d: %s carries %v", seed, c.ID, c.Faults)
				}
				for _, d := range c.After {
					if d >= c.ID {
						t.Fatalf("seed %d: %s is after %s, not an earlier change", seed, c.ID, d)
					}
				}
				if !slices.IsSorted(c.After) || len(slices.Compact(slices.Clone(c.After))) != len(c.After) {
					t.Fatalf("seed %d: %s after %v", seed, c.ID, c.After)
				}
			}
			for _, k := range AllFaults {
				if sc.Count(k) == 0 {
					t.Fatalf("seed %d, %d changes: no %s fault: %s", seed, n, k, sc)
				}
			}
			if red := sc.Count(RedCI); red > MaxRepairs(n) {
				t.Fatalf("seed %d: %d red-CI changes need more repairs than the cap of %d", seed, red, MaxRepairs(n))
			}
			for _, e := range sc.Events {
				if e.Kind == RateLimit && e.Ticks*TickMinutes != 30 {
					t.Fatalf("seed %d: rate-limit window is %d ticks, want 30 simulated minutes", seed, e.Ticks)
				}
			}
		}
	}
}

// A known-failing fault is left out of the scenario entirely.
func TestGenerateSkipsKnownFaults(t *testing.T) {
	t.Parallel()
	skip := map[FaultKind]string{RedBase: "#1", KilledWorker: "#2", AsyncMerge: "#3"}
	sc := Generate(Config{Seed: 3, Changes: 10, Skip: skip})
	for k := range skip {
		if n := sc.Count(k); n != 0 {
			t.Fatalf("skipped %s still appears %d times: %s", k, n, sc)
		}
	}
	if sc.Count(RedCI) == 0 || sc.Count(RateLimit) == 0 {
		t.Fatalf("other faults went missing: %s", sc)
	}
}

// A change's package imports its dependencies' packages, so the module
// builds with them and fails without them: the build dependency is real.
func TestGoFileNeedsItsDependency(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain")
	}
	dep := Change{ID: "c01", Issue: 1}
	top := Change{ID: "c02", Issue: 2, After: []string{"c01"}}
	write := func(dir string, cs ...Change) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(GoMod), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, c := range cs {
			p, body := GoFile(c)
			if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, p), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	build := func(dir string) error {
		cmd := exec.Command("go", "build", "./...")
		cmd.Dir, cmd.Env = dir, append(os.Environ(), "GOTOOLCHAIN=local", "GOFLAGS=-mod=mod")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Logf("go build: %s", out)
		}
		return err
	}
	with, without := t.TempDir(), t.TempDir()
	write(with, dep, top)
	write(without, top)
	if err := build(with); err != nil {
		t.Fatalf("c02 with c01 doesn't build: %v", err)
	}
	if err := build(without); err == nil {
		t.Fatal("c02 built without c01")
	}
}

// The red marker fails CI and the repair script's find removes it.
func TestGradeMarkers(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if got := GradeMarkers(dir); got != "" {
		t.Fatalf("clean tree graded %q", got)
	}
	for _, id := range []string{"c02", "c01"} {
		if err := os.MkdirAll(filepath.Join(dir, id), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id, RedMarker), []byte("TestSoak"+strings.ToUpper(id)+" fails\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := GradeMarkers(dir), "--- FAIL: TestSoakC01 fails\n--- FAIL: TestSoakC02 fails"; got != want {
		t.Fatalf("GradeMarkers = %q, want %q", got, want)
	}
}

// The dispatcher finds the change from the task branch's slug, which the
// issue title makes.
func TestDispatchFindsChangeFromBranch(t *testing.T) {
	t.Parallel()
	if !strings.HasPrefix(IssueTitle(Change{ID: "c07"}), "c07 soak") {
		t.Fatal("the issue title no longer leads with the change id")
	}
	script := DispatchScript("/s")
	for br, want := range map[string]string{
		"saddle/t12-c07-soak-change":     "c07",
		"saddle/t3-c10-soak-change":      "c10",
		"saddle/t14-fix-ci-for-t5-c07-s": "",
	} {
		out, err := exec.Command("sh", "-c", `br="$1"; printf '%s\n' "$br" | sed -nE 's#^.*/t[0-9]+-(c[0-9]+)-soak.*$#\1#p'`, "sh", br).Output()
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(out)); got != want {
			t.Errorf("branch %s -> %q, want %q", br, got, want)
		}
	}
	if !strings.Contains(script, `s#^.*/t[0-9]+-(c[0-9]+)-soak.*$#\1#p`) {
		t.Fatal("DispatchScript's pattern drifted from the one tested here")
	}
}

func TestIssueBody(t *testing.T) {
	t.Parallel()
	sc := Scenario{Changes: []Change{{ID: "c01", Issue: 1}, {ID: "c02", Issue: 2}, {ID: "c03", Issue: 3, After: []string{"c01", "c02"}, Faults: []FaultKind{ExcludedAdapter}}}}
	body := IssueBody(sc.Changes[2], sc, "codex")
	for _, want := range []string{"claims: c03/**", "after: #1, #2", "Provider: codex", "Difficulty: mechanical"} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
}

func v(k invariants.Kind, subject string) invariants.Violation {
	return invariants.Violation{Kind: k, Subject: subject, Detail: "d"}
}

// The ledger turns per-tick violations into spans, and Over flags a span
// longer than its budget or still open at the end.
func TestLedgerSpans(t *testing.T) {
	t.Parallel()
	var l Ledger
	l.Observe(0, []invariants.Violation{v(invariants.PRDrift, "t1")})
	l.Observe(1, []invariants.Violation{v(invariants.PRDrift, "t1"), v(invariants.Latched, "x")})
	l.Observe(2, []invariants.Violation{v(invariants.Latched, "x")})
	l.Observe(3, nil)
	l.Observe(4, []invariants.Violation{v(invariants.PRDrift, "t1")})
	got := l.All()
	want := []Span{
		{Key: "pr_drift t1", Kind: invariants.PRDrift, From: 0, To: 1, Detail: "d"},
		{Key: "latched x", Kind: invariants.Latched, From: 1, To: 2, Detail: "d"},
		{Key: "pr_drift t1", Kind: invariants.PRDrift, From: 4, To: 4, Open: true, Detail: "d"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("spans = %+v\nwant %+v", got, want)
	}
	over := l.Over(Budgets{invariants.PRDrift: 5, invariants.Latched: 1})
	if len(over) != 2 || over[0].Key != "latched x" || !over[1].Open {
		t.Fatalf("Over = %+v, want the two-tick latch and the open drift", over)
	}
	if over := l.Over(Budgets{invariants.PRDrift: 5, invariants.Latched: 2}); len(over) != 1 {
		t.Fatalf("Over = %+v, want only the open drift", over)
	}
}

// Failures names each way a run misses the bar.
func TestResultFailures(t *testing.T) {
	t.Parallel()
	sc := Generate(Config{Seed: 1, Changes: 10})
	ok := Result{Scenario: sc, Ledger: &Ledger{}}
	for _, c := range sc.Changes {
		ok.Outcomes = append(ok.Outcomes, Outcome{Change: c.ID, State: Merged})
	}
	ok.Outcomes[3].State = Dropped
	ok.Repairs = []Repair{{Task: "t11", For: "t2", Checks: []string{"ci"}}}
	if f := ok.Failures(DefaultBudgets); len(f) != 0 {
		t.Fatalf("a clean run failed: %v", f)
	}

	bad := ok
	bad.Outcomes = append([]Outcome(nil), ok.Outcomes...)
	bad.Outcomes[0].State = Stuck
	bad.Repairs = []Repair{{Task: "t11", For: "t2", Checks: []string{"saddle/base"}}, {Task: "t12", For: "t3"}}
	bad.Actions = []string{"decision card left for a human"}
	bad.Ledger = &Ledger{}
	bad.Ledger.Observe(0, []invariants.Violation{v(invariants.SelfRepair, "t11")})
	f := strings.Join(bad.Failures(DefaultBudgets), "\n")
	for _, want := range []string{"c01", "never reached a terminal state", "human or orchestrator action", "self_repair t11", "2 repairs for 10 changes, over the cap of 1", "targets saddle-owned check saddle/base"} {
		if !strings.Contains(f, want) {
			t.Errorf("failures lack %q:\n%s", want, f)
		}
	}
}
