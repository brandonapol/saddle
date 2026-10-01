package cli

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/gitx"
)

func TestRefguardCmd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SADDLE_TRAIN", "")
	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "one"},
		{"branch", "saddle/integration"},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "two"},
	} {
		if _, err := gitx.Run(root, args...); err != nil {
			t.Fatal(err)
		}
	}
	one, err := gitx.Run(root, "rev-parse", "HEAD~")
	if err != nil {
		t.Fatal(err)
	}
	two, err := gitx.Run(root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	run := func(state string) error {
		cmd := Root()
		cmd.SetArgs([]string{"refguard", state})
		cmd.SetIn(strings.NewReader(one + " " + two + " refs/heads/saddle/integration\n"))
		cmd.SetOut(new(strings.Builder))
		cmd.SetErr(new(strings.Builder))
		return cmd.Execute()
	}
	t.Setenv("SADDLE_TASK", "t2")
	if err := run("prepared"); err == nil || !strings.Contains(err.Error(), "only the merge train") {
		t.Fatalf("t2 moving integration: err = %v", err)
	}
	if err := run("committed"); err != nil {
		t.Fatalf("committed state: %v", err)
	}
	t.Setenv("SADDLE_TRAIN", "1")
	if err := run("prepared"); err != nil {
		t.Fatalf("train moving integration: %v", err)
	}
}

func TestRefguardPrePushCmd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SADDLE_TRAIN", "")
	root := t.TempDir()
	if _, err := gitx.Run(root, "init", "-q", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	const sha = "1111111111111111111111111111111111111111"
	const zero = "0000000000000000000000000000000000000000"
	run := func(ref string) error {
		cmd := Root()
		cmd.SetArgs([]string{"refguard", "pre-push"})
		cmd.SetIn(strings.NewReader("HEAD " + sha + " " + ref + " " + zero + "\n"))
		cmd.SetOut(new(strings.Builder))
		cmd.SetErr(new(strings.Builder))
		return cmd.Execute()
	}
	t.Setenv("SADDLE_TASK", "t2")
	if err := run("refs/heads/saddle/t2-x"); err == nil || !strings.Contains(err.Error(), "only the merge train pushes") {
		t.Fatalf("t2 pushing its own branch: err = %v", err)
	}
	if err := run("refs/heads/feature"); err != nil {
		t.Fatalf("t2 pushing a non-saddle branch: %v", err)
	}
	t.Setenv("SADDLE_TRAIN", "1")
	if err := run("refs/heads/saddle/t2-x"); err != nil {
		t.Fatalf("train pushing: %v", err)
	}
}
