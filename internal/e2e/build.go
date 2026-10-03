// Package e2e is saddle's end-to-end harness (#178). A World is a hermetic
// sandbox: a temp HOME, a repo with a bare origin, a fake GitHub behind a gh
// shim (package fakegh), a scripted fake agent standing in for Claude Code
// (package fakeagent) and a private tmux server. Tests drive the real saddle
// binary through it and wait on conditions, never on fixed sleeps.
//
// The journeys live in files tagged e2e; run them with `make test/e2e`.
package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DefaultTags are the build tags `make` compiles saddle with; E2E_GO_TAGS
// overrides them.
const DefaultTags = "grammar_subset grammar_subset_go grammar_subset_python"

// Bins are the binaries a World runs.
type Bins struct {
	Dir    string // holds saddle and gh; put it first on PATH
	Saddle string
	GH     string
	Agent  string // the fake agent
}

// Build compiles saddle, the gh shim and the fake agent into dir.
func Build(dir string) (Bins, error) {
	b := Bins{Dir: dir, Saddle: filepath.Join(dir, "saddle"), GH: filepath.Join(dir, "gh"),
		Agent: filepath.Join(dir, "fakeagent")}
	root, err := moduleRoot()
	if err != nil {
		return b, err
	}
	tags := os.Getenv("E2E_GO_TAGS")
	if tags == "" {
		tags = DefaultTags
	}
	for out, pkg := range map[string]string{
		b.Saddle: "./cmd/saddle",
		b.GH:     "./internal/e2e/fakegh/cmd/gh",
		b.Agent:  "./internal/e2e/fakeagent/cmd/fakeagent",
	} {
		cmd := exec.Command("go", "build", "-tags", tags, "-o", out, pkg)
		cmd.Dir = root
		if o, err := cmd.CombinedOutput(); err != nil {
			return b, fmt.Errorf("go build %s: %w\n%s", pkg, err, o)
		}
	}
	return b, nil
}

func moduleRoot() (string, error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", err
	}
	mod := strings.TrimSpace(string(out))
	if mod == "" || mod == os.DevNull {
		return "", fmt.Errorf("not in a Go module")
	}
	return filepath.Dir(mod), nil
}
