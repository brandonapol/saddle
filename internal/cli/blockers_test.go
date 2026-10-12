package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
)

type listBlockersGH struct {
	app.BlockerGitHub
	issues []app.BlockerIssue
}

func (g listBlockersGH) Issues(string) ([]app.BlockerIssue, error) { return g.issues, nil }

// #407: saddle blockers lists open blocker issues, most seen first, and
// handfix records a hand fix event for the filer.
func TestBlockersCommand(t *testing.T) {
	a := automergeRepo(t)
	t.Chdir(a.Root)
	body := func(sig string, n int) string {
		return fmt.Sprintf("<!-- saddle-blocker-signature: %s -->\n<!-- saddle-blocker-count: %d -->\n", sig, n)
	}
	g := listBlockersGH{}
	old := blockersGH
	t.Cleanup(func() { blockersGH = old })
	blockersGH = func(*app.App) app.BlockerGitHub { return g }

	out, err := runCmd(t, blockersCmd())
	if err != nil || !strings.Contains(out, "No open blocker issues") {
		t.Fatalf("empty: %v\n%s", err, out)
	}
	g.issues = []app.BlockerIssue{
		{Number: 11, Title: "CI red: lint", State: "OPEN", Body: body("ci_red:lint", 2)},
		{Number: 12, Title: "History check: GitGuardian", State: "OPEN", Body: body("history_check:gitguardian", 5)},
	}
	out, err = runCmd(t, blockersCmd())
	if err != nil {
		t.Fatal(err)
	}
	i, j := strings.Index(out, "#12"), strings.Index(out, "#11")
	if i < 0 || j < 0 || i > j || !strings.Contains(out, "5×") || !strings.Contains(out, "history_check:gitguardian") {
		t.Fatalf("list:\n%s", out)
	}

	if _, err := runCmd(t, blockersCmd(), "handfix", "t1", "reset", "the", "train", "row"); err != nil {
		t.Fatal(err)
	}
	es, err := a.Store.Events(10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range es {
		found = found || e.Kind == app.EventHandFix && e.Task == "t1" && e.Data == "reset the train row"
	}
	if !found {
		t.Fatalf("hand fix not recorded: %+v", es)
	}
	if _, err := runCmd(t, blockersCmd(), "handfix", "t404", "x"); err == nil {
		t.Fatal("handfix for an unknown task succeeded")
	}
}
