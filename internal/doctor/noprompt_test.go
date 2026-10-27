package doctor

import (
	"os"
	"path/filepath"
	"testing"
)

// doctor's push check pushes to whatever the remote is. On a host with no
// credentials for it (a GitLab-only VM whose remote is on github.com, say)
// it must report a warning, not stop at "Username for 'https://github.com'".
func TestPushCheckNeverPromptsForCredentials(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\necho \"$GIT_TERMINAL_PROMPT|$GCM_INTERACTIVE|$SADDLE_TRAIN\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("GIT_TERMINAL_PROMPT", "")
	t.Setenv("GCM_INTERACTIVE", "")
	out, err := system(t.TempDir()).TrainGit("push", "--dry-run", "origin", "HEAD:"+pushScratch)
	if err != nil {
		t.Fatal(err)
	}
	if out != "0|never|1" {
		t.Fatalf("push check ran git with env %q, want 0|never|1", out)
	}
}
