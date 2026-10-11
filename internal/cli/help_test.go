package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestRootHelpPointsAtGlossary(t *testing.T) {
	var out bytes.Buffer
	root := Root()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimRight(out.String(), "\n")
	lines := strings.Split(got, "\n")
	last := lines[len(lines)-1]
	if !strings.Contains(last, "docs/GLOSSARY.md") {
		t.Fatalf("last line of root help = %q, want it to mention docs/GLOSSARY.md", last)
	}
}

func TestSubcommandHelpHasNoGlossaryLine(t *testing.T) {
	var out bytes.Buffer
	root := Root()
	root.SetOut(&out)
	root.SetArgs([]string{"help", "version"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "GLOSSARY.md") {
		t.Fatalf("subcommand help should not carry the glossary line:\n%s", out.String())
	}
}
