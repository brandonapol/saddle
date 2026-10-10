package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOrchestratorSettingsDiagnosesInvalidAndNearMisses(t *testing.T) {
	f := healthy(t)
	dir := filepath.Join(f.root, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "settings.local.json")
	for _, tc := range []struct {
		text, want string
		status     Status
	}{
		{`{"permissions":`, "settings.local.json", Fail},
		{`{"permissions":{"allow":["mcp_saddle_prs"]}}`, "mcp_saddle_prs", Warn},
		{`{"enabledMcpjsonServers":["mcp__saddle__prs"]}`, "mcp__saddle__prs", Warn},
	} {
		if err := os.WriteFile(path, []byte(tc.text), 0o600); err != nil {
			t.Fatal(err)
		}
		r := (&run{env: f}).orchAllow()
		if r.Status != tc.status || !strings.Contains(r.Detail, tc.want) {
			t.Fatalf("%s: %+v", tc.text, r)
		}
	}
}

func TestDoctorFixMergesAllowlistAndPreservesSettings(t *testing.T) {
	f := healthy(t)
	f.allow = nil
	dir := filepath.Join(f.root, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "settings.local.json")
	if err := os.WriteFile(path, []byte(`{"permissions":{"allow":["Read"],"deny":["Bash(rm:*)"]},"enableAllProjectMcpServers":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Fix(f, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Bash(saddle:*)", "mcp__saddle", "Read", "Bash(rm:*)", "enableAllProjectMcpServers"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("settings missing %q: %s", want, b)
		}
	}
	before := string(b)
	_, err = Fix(f, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(path)
	if err != nil || string(b) != before {
		t.Fatalf("not idempotent: %s, %v", b, err)
	}
}

func TestDoctorFixDoesNotOverwriteMalformedSettings(t *testing.T) {
	f := healthy(t)
	f.allow = nil
	dir := filepath.Join(f.root, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "settings.local.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Fix(f, func() error { return nil })
	if err == nil {
		t.Fatal("malformed settings must require repair")
	}
	b, readErr := os.ReadFile(path)
	if readErr != nil || string(b) != "{" {
		t.Fatalf("settings overwritten: %s, %v", b, readErr)
	}
}
