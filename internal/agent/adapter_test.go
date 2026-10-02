package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeAdapterLaunchesLikeWrite(t *testing.T) {
	l := Launch{Root: "/repo", Bin: "/bin/saddle", Task: "t3", Title: "x", Dir: "/repo/wt", Model: "opus",
		Cmd: "claude", Brief: "brief", Prompt: "go", RunDir: t.TempDir()}
	want, err := l.Write()
	if err != nil {
		t.Fatal(err)
	}
	wantScript, _ := os.ReadFile(filepath.Join(l.RunDir, "launch.sh"))
	var a Adapter = Claude{}
	got, err := a.Launch(l)
	if err != nil {
		t.Fatal(err)
	}
	gotScript, _ := os.ReadFile(filepath.Join(l.RunDir, "launch.sh"))
	if got != want || string(gotScript) != string(wantScript) {
		t.Errorf("adapter launch differs from Launch.Write:\n%s\n%s", got, gotScript)
	}
	if !a.Hooks() {
		t.Error("claude runs saddle hook")
	}
	if a.Usage().Agent != "claude" {
		t.Errorf("usage agent %q", a.Usage().Agent)
	}
	if got := a.Inject("1. [action] rebase"); !strings.Contains(got, "new notices") || strings.Contains(got, "rebase") {
		t.Errorf("claude gets notices from its hook, so inject only wakes it: %q", got)
	}
}

func TestClaudeTranscriptPath(t *testing.T) {
	dir := t.TempDir()
	c := Claude{ConfigDir: dir}
	want := filepath.Join(dir, "projects", "-repo-wt", "s1.jsonl")
	if got := c.Usage().Transcript("/repo/wt", "", "s1"); got != want {
		t.Errorf("missing transcript: got %q want %q", got, want)
	}
	// A transcript under another project dir is still found by session id.
	other := filepath.Join(dir, "projects", "elsewhere", "s1.jsonl")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := c.Usage().Transcript("/repo/wt", "", "s1"); got != other {
		t.Errorf("got %q want %q", got, other)
	}
}

func TestByName(t *testing.T) {
	for name, want := range map[string]string{"": "claude", "claude": "claude", "codex": "codex", "grok": "grok"} {
		a, err := ByName(name)
		if err != nil || a.Name() != want {
			t.Errorf("ByName(%q) = %v, %v", name, a, err)
		}
	}
	if _, err := ByName("nope"); err == nil || !strings.Contains(err.Error(), "claude") {
		t.Errorf("unknown adapter should list known ones: %v", err)
	}
}
