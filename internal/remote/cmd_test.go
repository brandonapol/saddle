package remote

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
)

func run(t *testing.T, open func() (*app.App, error), args ...string) (string, error) {
	t.Helper()
	cmd := Command(open)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func noOpen() (*app.App, error) { return nil, errors.New("open must not be called") }

func TestServeRefusesWhenOff(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, err := run(t, noOpen, "serve")
	if err == nil || !strings.Contains(err.Error(), "remote control is off") {
		t.Fatalf("serve with no config = %v, want it off", err)
	}
}

func TestServeRefusesNonLoopback(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	writeFile(t, filepath.Join(cfg, "saddle", "config.toml"), "[remote]\nenabled = true\nlisten = \"0.0.0.0:7431\"\n")
	_, err := run(t, noOpen, "serve")
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("serve on 0.0.0.0 = %v, want refused", err)
	}
}

func TestTokenCommands(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvDir, dir)
	out, err := run(t, noOpen, "token", "create", "phone", "--scope", "act", "--ttl", "2h")
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.TrimSpace(out)
	tok, err := NewTokens(dir).Verify(secret)
	if err != nil {
		t.Fatalf("printed secret doesn't verify: %v", err)
	}
	if joinScopes(tok.Scopes) != "read,act" {
		t.Fatalf("scopes = %v", tok.Scopes)
	}
	out, err = run(t, noOpen, "token", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "phone") || !strings.Contains(out, "live") || strings.Contains(out, secret) {
		t.Fatalf("list = %q", out)
	}
	if _, err := run(t, noOpen, "token", "revoke", "phone"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewTokens(dir).Verify(secret); !errors.Is(err, ErrBadToken) {
		t.Fatalf("revoked secret still verifies: %v", err)
	}
	if _, err := run(t, noOpen, "token", "create", "x", "--scope", "root"); err == nil {
		t.Fatal("create accepted an unknown scope")
	}
}
