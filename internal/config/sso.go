package config

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/brandonapol/saddle/internal/sso"
)

// SSO is one [sso.<name>] table: an OIDC provider saddle logs in to once
// and whose tokens feed an agent CLI (#758).
type SSO struct {
	// Issuer is the OIDC issuer URL; endpoints come from its discovery
	// document. https, except loopback hosts.
	Issuer string `toml:"issuer"`
	// ClientID is a public client with the device authorization grant on.
	ClientID string `toml:"client_id"`
	// Scopes default to openid offline_access.
	Scopes []string `toml:"scopes"`
	// Audience is sent as audience= (Okta custom servers, Auth0).
	Audience string `toml:"audience"`
	// Token is "access" (default) or "id": which token consumers get.
	Token string `toml:"token"`
	// CodexProvider is the codex model_provider id this login feeds. Codex
	// workers and a codex orchestrator then run with that provider and get
	// their bearer token from saddle sso token.
	CodexProvider string `toml:"codex_provider"`
	// BaseURL, when set, defines the codex provider's base_url too, so it
	// needs no entry in codex's own config.toml.
	BaseURL string `toml:"base_url"`
}

// SSOMap is the [sso.<name>] tables, by name.
type SSOMap map[string]SSO

var ssoKeys = []string{"issuer", "client_id", "scopes", "audience", "token", "codex_provider", "base_url"}

// UnmarshalTOML decodes [sso.<name>] tables strictly: a typo in an auth
// setting should fail loudly, not silently fall back to OpenAI auth.
// Tables from a later config file replace earlier ones of the same name.
func (m *SSOMap) UnmarshalTOML(v any) error {
	tbl, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("sso: want a table of [sso.<name>] tables")
	}
	if *m == nil {
		*m = SSOMap{}
	}
	for name, raw := range tbl {
		t, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("sso.%s: want a table", name)
		}
		var s SSO
		for k, v := range t {
			if !slices.Contains(ssoKeys, k) {
				return fmt.Errorf("sso.%s: unknown key %q (want %s)", name, k, strings.Join(ssoKeys, ", "))
			}
			if k == "scopes" {
				l, err := stringList("sso."+name, k, v)
				if err != nil {
					return fmt.Errorf("sso.%s.scopes: want an array of strings", name)
				}
				s.Scopes = l
				continue
			}
			str, ok := v.(string)
			if !ok {
				return fmt.Errorf("sso.%s.%s: want a string", name, k)
			}
			str = strings.TrimSpace(str)
			switch k {
			case "issuer":
				s.Issuer = str
			case "client_id":
				s.ClientID = str
			case "audience":
				s.Audience = str
			case "token":
				s.Token = strings.ToLower(str)
			case "codex_provider":
				s.CodexProvider = str
			case "base_url":
				s.BaseURL = str
			}
		}
		(*m)[name] = s
	}
	return nil
}

// checkSSO validates the [sso] tables and fills defaults.
func checkSSO(m SSOMap) error {
	codex := ""
	for _, name := range m.Names() {
		s := m[name]
		if s.Issuer == "" || s.ClientID == "" {
			return fmt.Errorf("sso.%s: issuer and client_id are required", name)
		}
		if err := sso.CheckIssuer(s.Issuer); err != nil {
			return fmt.Errorf("sso.%s: %w", name, err)
		}
		switch s.Token {
		case "":
			s.Token = sso.TokenAccess
		case sso.TokenAccess, sso.TokenID:
		default:
			return fmt.Errorf("sso.%s.token %q: want access or id", name, s.Token)
		}
		if s.BaseURL != "" && s.CodexProvider == "" {
			return fmt.Errorf("sso.%s.base_url: set codex_provider too", name)
		}
		if s.CodexProvider != "" {
			if codex != "" {
				return fmt.Errorf("sso.%s and sso.%s both set codex_provider: codex runs one provider", codex, name)
			}
			codex = name
		}
		m[name] = s
	}
	return nil
}

// Names lists the [sso] tables in order.
func (m SSOMap) Names() []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Codex is the [sso] table feeding codex, if any.
func (m SSOMap) Codex() (name string, s SSO, ok bool) {
	for _, n := range m.Names() {
		if m[n].CodexProvider != "" {
			return n, m[n], true
		}
	}
	return "", SSO{}, false
}

// Provider is the sso package's view of table name.
func (s SSO) Provider(name string) sso.Provider {
	return sso.Provider{Name: name, Issuer: s.Issuer, ClientID: s.ClientID, Scopes: s.Scopes, Audience: s.Audience, Token: s.Token}
}
