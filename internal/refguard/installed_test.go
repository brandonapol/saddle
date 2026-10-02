package refguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestInstalledReportsHookState(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	hooks, err := Installed(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(hooks) != 2 || hooks[0].Present || hooks[1].Present {
		t.Fatalf("fresh repo: %+v", hooks)
	}

	bin := "/opt/it's/saddle"
	if err := Install(root, bin); err != nil {
		t.Fatal(err)
	}
	hooks, err = Installed(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hooks {
		if !h.Present || !h.Saddle || h.Bin != bin {
			t.Fatalf("installed: %+v", h)
		}
	}

	foreign := filepath.Join(filepath.Dir(hooks[1].Path), "pre-push")
	if err := os.WriteFile(foreign, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	hooks, err = Installed(root)
	if err != nil {
		t.Fatal(err)
	}
	if h := hooks[1]; h.Name != "pre-push" || !h.Present || h.Saddle || h.Bin != "" {
		t.Fatalf("foreign pre-push: %+v", h)
	}
}
