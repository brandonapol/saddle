package contract

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureDir = "testdata"

// TestContract runs every scenario against its recorded fixture and against
// fakegh. Each must come to the scenario's outcome, so GitHub and the fake
// agree. A fakegh failure is a fakegh bug; a fixture failure means GitHub
// (or saddle's forge) changed.
func TestContract(t *testing.T) {
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Run("fixture", func(t *testing.T) {
				fx, err := LoadFixture(filepath.Join(fixtureDir, sc.name+".json"))
				if err != nil {
					t.Fatal(err)
				}
				if fx.Scenario != sc.name {
					t.Fatalf("fixture names scenario %q", fx.Scenario)
				}
				rec := NewRecorder(fx)
				got := OutcomeOf(sc.act(context.Background(), GitHub(rec.Run), fx.Params))
				if err := rec.Err(); err != nil {
					t.Errorf("calls diverged from the recording:\n%v", err)
				}
				if got != sc.want {
					t.Errorf("GitHub (recorded): %v, want %v", got, sc.want)
				}
			})
			t.Run("fakegh", func(t *testing.T) {
				f := NewFake(t)
				p := sc.fake(f)
				got := OutcomeOf(sc.act(context.Background(), GitHub(f.Run), p))
				if got != sc.want {
					t.Errorf("fakegh: %v, want %v (GitHub's outcome; fix fakegh)", got, sc.want)
				}
			})
		})
	}
}

// Every fixture belongs to a scenario, so a recording can't sit unchecked.
func TestEveryFixtureHasAScenario(t *testing.T) {
	names := map[string]bool{}
	for _, sc := range scenarios {
		if names[sc.name] {
			t.Errorf("scenario %q is listed twice", sc.name)
		}
		names[sc.name] = true
	}
	ents, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if ok && !names[name] {
			t.Errorf("%s has no scenario", e.Name())
		}
	}
}

// The recorder refuses a call it didn't record, and reports recorded calls
// nobody made, so a fixture pins the whole conversation.
func TestRecorderPinsTheCalls(t *testing.T) {
	rec := NewRecorder(Fixture{Calls: []Call{
		{Args: []string{"pr", "view", "1"}, Stdout: "{}\n"},
		{Args: []string{"pr", "checks", "1"}, Stdout: "[]", Stderr: "pending", Exit: 8},
		{Args: []string{"pr", "close", "1"}},
	}})
	ctx := context.Background()
	if out, err := rec.Run(ctx, "pr", "view", "1"); err != nil || out != "{}" {
		t.Fatalf("recorded call = %q, %v", out, err)
	}
	out, err := rec.Run(ctx, "pr", "checks", "1")
	if out != "[]" || err == nil || err.Error() != "gh pr checks: exit status 8: pending" {
		t.Fatalf("exit 8 = %q, %v", out, err)
	}
	if _, err := rec.Run(ctx, "pr", "merge", "1"); err == nil {
		t.Fatal("an unrecorded call answered")
	}
	err = rec.Err()
	if err == nil || !strings.Contains(err.Error(), "gh pr merge 1, recorded gh pr close 1") || !strings.Contains(err.Error(), "not made: gh pr close 1") {
		t.Fatalf("Err = %v", err)
	}
}

// TestRecordFixtures is make contract-record: it re-records every fixture
// against a real repo. That needs the network and a token, so make check
// never runs it, and the recorder itself is #824's canary.
func TestRecordFixtures(t *testing.T) {
	if os.Getenv("SADDLE_CONTRACT_RECORD") == "" {
		t.Skip("set SADDLE_CONTRACT_RECORD=1 (make contract-record) to re-record the fixtures")
	}
	if os.Getenv("GH_TOKEN") == "" || os.Getenv("SADDLE_CONTRACT_REPO") == "" {
		t.Fatal("recording needs GH_TOKEN and SADDLE_CONTRACT_REPO (owner/name of a scratch repo with Stacked PRs)")
	}
	t.Fatal("recording against GitHub is not implemented yet: #824's canary records these fixtures")
}
