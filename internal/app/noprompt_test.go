package app

import (
	"os"
	"path/filepath"
	"testing"
)

// The train fetches and pushes unattended; a remote without stored
// credentials must fail the git call, not block it on a terminal prompt.
func TestTrainGitNeverPromptsForCredentials(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\necho \"$GIT_TERMINAL_PROMPT|$GCM_INTERACTIVE|$SADDLE_TRAIN\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("GIT_TERMINAL_PROMPT", "")
	t.Setenv("GCM_INTERACTIVE", "")
	out, err := trainGit(t.TempDir(), "push", "origin", "HEAD:refs/heads/x")
	if err != nil {
		t.Fatal(err)
	}
	if out != "0|never|1" {
		t.Fatalf("train ran git with env %q, want 0|never|1", out)
	}
}
