package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexBridgeCmdRunsCodexBridge(t *testing.T) {
	run := t.TempDir()
	fake := filepath.Join(run, "codex")
	if err := os.WriteFile(fake, []byte(`#!/bin/sh
cat >/dev/null
printf '%s\n' '{"type":"thread.started","thread_id":"thread-1"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"from codex"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`), 0o755); err != nil {
		t.Fatal(err)
	}
	spec, err := json.Marshal(map[string]string{"cmd": fake, "dir": run})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, "codex.json"), spec, 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := codexBridgeCmd()
	cmd.SetArgs([]string{run})
	cmd.SetIn(strings.NewReader("{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"hello\"}}\n"))
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "from codex") {
		t.Fatalf("codex bridge output = %q", out.String())
	}
}
