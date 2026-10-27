package gitx

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeGit puts a git on PATH that prints the prompt-related environment it
// was started with, so tests can see what saddle hands git.
func fakeGit(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\necho \"$GIT_TERMINAL_PROMPT|$GCM_INTERACTIVE\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("GIT_TERMINAL_PROMPT", "")
	t.Setenv("GCM_INTERACTIVE", "")
}

// A fetch or push against an HTTPS remote without stored credentials must
// fail, not stop saddle (or doctor) at a "Username for 'https://...'" prompt.
func TestRunNeverPromptsForCredentials(t *testing.T) {
	fakeGit(t)
	out, err := Run(t.TempDir(), "fetch", "origin")
	if err != nil {
		t.Fatal(err)
	}
	if out != "0|never" {
		t.Fatalf("git ran with prompt env %q, want 0|never", out)
	}
}
