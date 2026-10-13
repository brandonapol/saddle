package cli

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/store"
)

var glossaryHeading = regexp.MustCompile(`(?m)^### (.+)$`)

// glossaryTerms reads the headings of docs/GLOSSARY.md, in file order.
func glossaryTerms(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("../../docs/GLOSSARY.md")
	if err != nil {
		t.Fatal(err)
	}
	var terms []string
	for _, m := range glossaryHeading.FindAllStringSubmatch(string(b), -1) {
		terms = append(terms, strings.TrimSpace(m[1]))
	}
	return terms
}

// The glossary must define every command that `saddle help` lists.
func TestGlossaryCoversEveryCommand(t *testing.T) {
	terms := glossaryTerms(t)
	for _, c := range Root().Commands() {
		if c.Hidden || c.Name() == "help" {
			continue
		}
		if !slices.Contains(terms, c.Name()) {
			t.Errorf("docs/GLOSSARY.md has no entry for command %q; add a \"### %s\" section", c.Name(), c.Name())
		}
	}
}

// The glossary must define every task status and train entry state.
func TestGlossaryCoversEveryState(t *testing.T) {
	terms := glossaryTerms(t)
	states := []string{
		store.Running, store.Idle, store.NeedsYou, store.Done, store.Conflict,
		store.Landed, store.Killed, app.StatusFailed, app.StatusPaused, app.StatusOrphaned,
		store.Queued, store.TrainOK, store.TestFailed, store.OnHold,
		app.TrainMerged, "superseded",
	}
	for _, s := range states {
		if !slices.Contains(terms, s) {
			t.Errorf("docs/GLOSSARY.md has no entry for state %q; add a \"### %s\" section", s, s)
		}
	}
}

func TestGlossaryIsAlphabetical(t *testing.T) {
	terms := glossaryTerms(t)
	for i := 1; i < len(terms); i++ {
		if strings.ToLower(terms[i-1]) > strings.ToLower(terms[i]) {
			t.Errorf("docs/GLOSSARY.md: %q should come after %q", terms[i-1], terms[i])
		}
	}
}
