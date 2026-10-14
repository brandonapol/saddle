package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/usage"
)

func TestGrokBridgeResumes(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "grok")
	script := `#!/bin/sh
prev=
prompt=
resume=
for a in "$@"; do
  if [ "$prev" = "--prompt-file" ]; then prompt=$(cat "$a"); fi
  if [ "$prev" = "--resume" ]; then resume=$a; fi
  prev=$a
done
sid=11111111-1111-4111-8111-111111111111
printf '%s\n' "{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"$sid\"}"
printf '%s\n' "{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"prompt=$prompt resume=$resume\"}]}}"
printf '%s\n' "{\"type\":\"result\",\"subtype\":\"success\",\"result\":\"ok\",\"session_id\":\"$sid\"}"
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := grokSpec{Cmd: fake, Args: []string{"--no-auto-update"}, Dir: dir}
	b, _ := json.Marshal(spec)
	if err := os.WriteFile(filepath.Join(dir, "grok.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	// The command chooses the bridge explicitly; stale run metadata must not
	// turn grok-bridge into codex-bridge.
	if err := (Launch{RunDir: dir}).record(usage.Codex); err != nil {
		t.Fatal(err)
	}
	in := strings.NewReader(strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"first"}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"second"}]}}`,
	}, "\n") + "\n")
	var out bytes.Buffer
	if err := RunGrokBridge(dir, in, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "prompt=first resume=") || strings.Contains(text, "prompt=first resume=1111") {
		t.Fatalf("first turn should not resume:\n%s", text)
	}
	if !strings.Contains(text, "prompt=second resume=11111111-1111-4111-8111-111111111111") {
		t.Fatalf("second turn did not resume:\n%s", text)
	}
}

// #263: grok can write an error result with no text, then exit non-zero with
// the reason on stderr. The bridge must pass that reason on, not a blank error.
func TestGrokBridgeKeepsStderrOnBlankErrorResult(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "grok")
	script := `#!/bin/sh
printf '%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"","session_id":""}'
echo "Not signed in. Run grok login." >&2
exit 1
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(grokSpec{Cmd: fake, Dir: dir})
	if err := os.WriteFile(filepath.Join(dir, "grok.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	in := strings.NewReader(`{"type":"user","message":{"role":"user","content":"hi"}}` + "\n")
	var out bytes.Buffer
	if err := RunGrokBridge(dir, in, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Not signed in. Run grok login.") {
		t.Fatalf("stderr reason lost:\n%s", out.String())
	}
}
