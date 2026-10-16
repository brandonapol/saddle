package fakecodex

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/usage"
)

// The fake answers exec --json like codex, and its rollout is one saddle's
// context reader and transcript lookup understand.
func TestExecTurnAndRollout(t *testing.T) {
	h := t.TempDir()
	t.Setenv("CODEX_HOME", h)
	t.Chdir(t.TempDir())
	var out, errb bytes.Buffer
	args := []string{"exec", "--json", "--skip-git-repo-check", "-c", `mcp_servers.saddle.command="/bin/saddle"`,
		"-c", `mcp_servers.saddle.env={SADDLE_ROOT="/r",SADDLE_TASK="t0"}`, "-m", "gpt-x", "-"}
	if code := Main(args, strings.NewReader("hello\nTOKENS 150000\n"), &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	for _, want := range []string{`"thread.started"`, `"thread_id":"0199fake-0001"`, `fakecodex heard: hello`, `"turn.completed"`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("events lack %s:\n%s", want, out.String())
		}
	}
	wd, _ := os.Getwd()
	p := agent.Codex{Home: h}.Usage().Transcript(wd, "", "0199fake-0001")
	if p == "" {
		t.Fatal("no rollout for the session")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var got usage.ContextReading
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if r, ok, err := usage.ParseCodexContext([]byte(l)); err == nil && ok && r.Prompt > 0 {
			got = r
		}
	}
	if got.Prompt != 150000 || got.Window != Window {
		t.Errorf("context reading = %+v, want 150000 of %d", got, Window)
	}
	if log, _ := os.ReadFile(filepath.Join(h, "fakecodex.log")); !strings.Contains(string(log), "hello") {
		t.Errorf("log lacks the prompt: %q", log)
	}
	// resume keeps the id.
	out.Reset()
	args = append(args[:len(args)-1], "resume", "0199fake-0001", "-")
	if code := Main(args, strings.NewReader("again"), &out, &errb); code != 0 || !strings.Contains(out.String(), `"thread_id":"0199fake-0001"`) {
		t.Errorf("resume: exit %d, %s", code, out.String())
	}
}
