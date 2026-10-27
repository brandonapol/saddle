package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func loadBody(t *testing.T, body string) (Config, error) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".saddle"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".saddle", "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(root)
}

func TestSSOParses(t *testing.T) {
	cfg, err := loadBody(t, `
[sso.corp]
issuer = "https://keycloak.example.com/realms/eng"
client_id = "saddle-cli"
scopes = ["openid", "offline_access", "gateway"]
audience = "api://gw"
codex_provider = "corp-gw"
base_url = "https://gw.example.com/v1"

[sso.okta]
issuer = "https://dev-1.okta.com/oauth2/default"
client_id = "0oa1"
token = "ID"
`)
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.SSO["corp"]
	if c.ClientID != "saddle-cli" || !slices.Equal(c.Scopes, []string{"openid", "offline_access", "gateway"}) ||
		c.Audience != "api://gw" || c.Token != "access" || c.BaseURL != "https://gw.example.com/v1" {
		t.Fatalf("corp = %+v", c)
	}
	if cfg.SSO["okta"].Token != "id" {
		t.Fatalf("okta token = %q", cfg.SSO["okta"].Token)
	}
	name, s, ok := cfg.SSO.Codex()
	if !ok || name != "corp" || s.CodexProvider != "corp-gw" {
		t.Fatalf("codex = %s %+v %v", name, s, ok)
	}
	if p := c.Provider("corp"); p.Issuer != c.Issuer || p.Name != "corp" {
		t.Fatalf("provider = %+v", p)
	}
}

func TestSSOUnsetIsEmpty(t *testing.T) {
	cfg, err := loadBody(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := cfg.SSO.Codex(); ok || len(cfg.SSO) != 0 {
		t.Fatalf("sso = %+v", cfg.SSO)
	}
}

func TestSSORejects(t *testing.T) {
	for want, body := range map[string]string{
		`unknown key "clientid"`:  "[sso.a]\nissuer = \"https://x.example.com\"\nclientid = \"c\"\n",
		"issuer and client_id":    "[sso.a]\nissuer = \"https://x.example.com\"\n",
		"want https":              "[sso.a]\nissuer = \"http://x.example.com\"\nclient_id = \"c\"\n",
		"want access or id":       "[sso.a]\nissuer = \"https://x.example.com\"\nclient_id = \"c\"\ntoken = \"refresh\"\n",
		"set codex_provider too":  "[sso.a]\nissuer = \"https://x.example.com\"\nclient_id = \"c\"\nbase_url = \"https://gw\"\n",
		"both set codex_provider": "[sso.a]\nissuer = \"https://x.example.com\"\nclient_id = \"c\"\ncodex_provider = \"p\"\n[sso.b]\nissuer = \"https://y.example.com\"\nclient_id = \"c\"\ncodex_provider = \"q\"\n",
		"scopes: want an array":   "[sso.a]\nissuer = \"https://x.example.com\"\nclient_id = \"c\"\nscopes = \"openid\"\n",
	} {
		_, err := loadBody(t, body)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v", want, err)
		}
	}
}
