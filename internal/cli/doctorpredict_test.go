package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/doctor"
)

func predictApp(t *testing.T) *app.App {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SADDLE_ROOT", "")
	t.Setenv("SADDLE_TASK", "")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	root := t.TempDir()
	gitRun(t, root, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", "-A")
	gitRun(t, root, "commit", "-qm", "init")
	a, err := app.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

var upRisks = []doctor.Risk{
	{Severity: doctor.High, Class: doctor.ClassDisk, What: "only 3.0 GB free for tmp (/tmp)", Fix: "run `saddle gc`"},
	{Severity: doctor.Medium, Class: doctor.ClassLoad, What: "concurrency 8 on 8 CPUs", Fix: "`saddle concurrency 4`"},
	{Severity: doctor.Low, Class: doctor.ClassClaudeCLI, What: "Claude Code can update itself", Fix: "export DISABLE_AUTOUPDATER=1"},
}

// #408: saddle up shows the top risks once, as one card, without blocking.
func TestUpPredictOpensOneCardWithoutBlocking(t *testing.T) {
	a := predictApp(t)
	release := make(chan struct{})
	start := time.Now()
	wait := upPredict(a, 5*time.Second, func() []doctor.Risk { <-release; return upRisks })
	if d := time.Since(start); d > time.Second {
		t.Fatalf("upPredict blocked for %s", d)
	}
	if cs, _ := a.OpenDecisions(); len(cs) != 0 {
		t.Fatalf("card before the prediction finished: %+v", cs)
	}
	close(release)
	wait()
	// A second saddle up with the same risks doesn't ping again.
	upPredict(a, 5*time.Second, func() []doctor.Risk { return upRisks })()

	cs, err := a.OpenDecisions()
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 {
		t.Fatalf("want one card, got %d: %+v", len(cs), cs)
	}
	c := cs[0]
	if c.Kind != decisionPredictedRisks || c.To != app.OrchestratorID || c.Default != 2 {
		t.Fatalf("card: %+v", c)
	}
	for _, want := range []string{"3.0 GB", "concurrency 8"} {
		if !strings.Contains(c.What, want) {
			t.Errorf("card What %q lacks %q", c.What, want)
		}
	}
	if strings.Contains(c.What, "update itself") {
		t.Errorf("Low risk on the card: %q", c.What)
	}
	if !c.Options[0].Recommended || c.Options[0].Action != "run `saddle gc`" {
		t.Errorf("a High risk should recommend its fix: %+v", c.Options)
	}
	ns, err := a.Store.PeekNotices(app.OrchestratorID, false)
	if err != nil {
		t.Fatal(err)
	}
	cards := 0
	for _, n := range ns {
		if strings.Contains(n.Text, "Before you walk away") {
			cards++
		}
	}
	if cards != 1 {
		t.Fatalf("want the card sent once, got %d notices: %+v", cards, ns)
	}
}

func TestUpPredictNothingWorthACard(t *testing.T) {
	a := predictApp(t)
	upPredict(a, time.Second, func() []doctor.Risk { return upRisks[2:] })()
	if cs, _ := a.OpenDecisions(); len(cs) != 0 {
		t.Fatalf("Low-only risks opened a card: %+v", cs)
	}
}

func TestUpPredictTimesOutQuietly(t *testing.T) {
	a := predictApp(t)
	block := make(chan struct{})
	defer close(block)
	upPredict(a, 10*time.Millisecond, func() []doctor.Risk { <-block; return upRisks })()
	if cs, _ := a.OpenDecisions(); len(cs) != 0 {
		t.Fatalf("card after a timeout: %+v", cs)
	}
}

func TestPredictedRisksCardValidates(t *testing.T) {
	c, ok := predictedRisksCard(upRisks)
	if !ok {
		t.Fatal("no card")
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.Headline, "1 of them High") {
		t.Errorf("headline %q", c.Headline)
	}
}
