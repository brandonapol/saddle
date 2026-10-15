package bench

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// benchDir is the repo's bench/ from this package.
const benchDir = "../../bench"

func scenarios(t *testing.T) []*Scenario {
	t.Helper()
	names, err := List(benchDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) < 4 {
		t.Fatalf("scenarios = %v, want at least 4", names)
	}
	var out []*Scenario
	for _, n := range names {
		sc, err := Load(benchDir, n)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, sc)
	}
	return out
}

// goTest runs the fixture's suite in dir.
func goTest(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go test in %s: %v\n%s", dir, err, out)
	}
}

func TestFixtureTestsPass(t *testing.T) {
	dir := t.TempDir()
	if err := CopyFixture(FixtureDir(benchDir), dir); err != nil {
		t.Fatal(err)
	}
	goTest(t, dir)
}

func TestScenariosLoadAndApplyToFixture(t *testing.T) {
	for _, sc := range scenarios(t) {
		for _, tk := range sc.Tickets {
			if tk.Claims == nil {
				t.Errorf("%s %s declares no claims", sc.Name, tk.Key())
			}
			c := Apply(DirTree(FixtureDir(benchDir)), tk.Edits)
			if len(c.Stale) > 0 || len(c.Followed) > 0 {
				t.Errorf("%s %s: edits don't apply to the fixture as written: stale %v, followed %v", sc.Name, tk.Key(), c.Stale, c.Followed)
			}
		}
	}
}

// Each ticket is valid work on its own: the fixture with only its edits
// still passes. Collisions come from running tickets together.
func TestEachTicketAlonePasses(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go test once per ticket")
	}
	for _, sc := range scenarios(t) {
		for _, tk := range sc.Tickets {
			t.Run(sc.Name+"/"+tk.Key(), func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				if err := CopyFixture(FixtureDir(benchDir), dir); err != nil {
					t.Fatal(err)
				}
				c := Apply(DirTree(dir), tk.Edits)
				if err := c.WriteTo(dir); err != nil {
					t.Fatal(err)
				}
				goTest(t, dir)
			})
		}
	}
}

// TestFourRemainingScenarios is #605 part 2: the four shapes #599 listed
// after the first six. TestScenarioOverlapMatchesForecast and
// TestEachTicketAlonePasses gate every scenario those names load.
func TestFourRemainingScenarios(t *testing.T) {
	names, err := List(benchDir)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
	}
	for _, name := range []string{"signature", "ci-red", "generated", "serial"} {
		if !have[name] {
			t.Errorf("missing scenario %s (have %v)", name, names)
			continue
		}
		if _, err := Load(benchDir, name); err != nil {
			t.Errorf("scenario %s: %v", name, err)
		}
	}
}

// TestSignatureScenarioBreaksWhenCombined is the stale assumption: each
// signature ticket passes on its own (TestEachTicketAlonePasses), and the
// two together do not.
func TestSignatureScenarioBreaksWhenCombined(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go test on the combined edits")
	}
	sc, err := Load(benchDir, "signature")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := CopyFixture(FixtureDir(benchDir), dir); err != nil {
		t.Fatal(err)
	}
	for _, tk := range sc.Tickets {
		c := Apply(DirTree(dir), tk.Edits)
		if len(c.Stale) > 0 || len(c.Followed) > 0 {
			t.Fatalf("%s: edits don't apply on top of the earlier tickets: stale %v, followed %v", tk.Key(), c.Stale, c.Followed)
		}
		if err := c.WriteTo(dir); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("signature tickets together passed; want a broken build\n%s", out)
	}
}

func TestScenarioOverlapMatchesForecast(t *testing.T) {
	for _, sc := range scenarios(t) {
		p := MakePlan(DirTree(FixtureDir(benchDir)), sc)
		if p.Overlap != sc.Overlap {
			t.Errorf("%s: scenario.toml expects overlap %s, internal/forecast says %s: %+v", sc.Name, sc.Overlap, p.Overlap, p.Pairs)
		}
	}
}

func TestLoadRejectsBadScenario(t *testing.T) {
	dir := t.TempDir()
	sd := filepath.Join(ScenariosDir(dir), "bad", "tickets")
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatal(err)
	}
	must := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must(filepath.Join(sd, "01.md"), "no title\n")
	must(filepath.Join(sd, "..", "scenario.toml"), "overlap = \"huge\"\nsafe_parallelism = 3\n[[ticket]]\nfile = \"01.md\"\nwait_for = [1]\n[[ticket.edit]]\npath = \"a\"\nfrom = \"b\"\nnew = \"x\"\n")
	_, err := Load(dir, "bad")
	if err == nil {
		t.Fatal("loaded a bad scenario")
	}
	for _, want := range []string{"overlap \"huge\"", "safe_parallelism 3", "no '# title'", "waits for #1", "take no new"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
	if _, err := Load(dir, "../x"); err == nil {
		t.Error("loaded a scenario outside the scenarios dir")
	}
}

func TestSplitTitle(t *testing.T) {
	title, body := splitTitle("# Do it\n\nclaims: a/**\n")
	if title != "Do it" || body != "claims: a/**\n" {
		t.Fatalf("splitTitle = %q, %q", title, body)
	}
}

// TestMakefileE2ERunsBench locks #605 part 1: TestBenchRunsControlAndACollision
// is an e2e test in this package, so make test/e2e has to start it.
func TestMakefileE2ERunsBench(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	var recipe strings.Builder
	in := false
	for _, line := range strings.Split(string(b), "\n") {
		switch {
		case strings.HasPrefix(line, "test/e2e:"):
			in = true
		case in && line != "" && !strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, "#"):
			in = false
		}
		if in {
			recipe.WriteString(line)
			recipe.WriteByte('\n')
		}
	}
	if !strings.Contains(recipe.String(), "./internal/bench/...") {
		t.Fatalf("make test/e2e does not run ./internal/bench/...:\n%s", recipe.String())
	}
}
