package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenCodeBridgeCmdRunsOpenCodeBridge(t *testing.T) {
	run := t.TempDir()
	fake := filepath.Join(run, "opencode")
	if err := os.WriteFile(fake, []byte(`#!/bin/sh
cat >/dev/null
printf '%s\n' '{"type":"text","sessionID":"ses_1","part":{"type":"text","text":"from opencode"}}'
`), 0o755); err != nil {
		t.Fatal(err)
	}
	spec, err := json.Marshal(map[string]string{"cmd": fake, "dir": run})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, "opencode-orchestrator.json"), spec, 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := openCodeBridgeCmd()
	cmd.SetArgs([]string{run})
	cmd.SetIn(strings.NewReader("{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"hello\"}}\n"))
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "from opencode") || !strings.Contains(out.String(), `"type":"result"`) {
		t.Fatalf("opencode bridge output = %q", out.String())
	}
}
