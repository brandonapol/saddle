package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/sso"
	"github.com/spf13/cobra"
)

// fakeIssuer answers discovery and device requests; its token endpoint
// leaves every sign-in pending.
func fakeIssuer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	var starts atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"token_endpoint": srv.URL + "/token", "device_authorization_endpoint": srv.URL + "/device"})
		case "/device":
			starts.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"device_code": "DEV-SECRET", "user_code": "ABCD-EFGH",
				"verification_uri": srv.URL + "/activate", "expires_in": 600, "interval": 5})
		default:
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &starts
}

func ssoTestCmd() *cobra.Command {
	c := &cobra.Command{}
	c.SetContext(context.Background())
	return c
}

// #758: a child whose login can't be renewed starts one sign-in, hands its
// URL and code back (codex shows the error; the poller tells the
// orchestrator), and never shows the device code.
func TestSSOTokenStartsOneSignIn(t *testing.T) {
	srv, starts := fakeIssuer(t)
	st := sso.Store{Dir: filepath.Join(t.TempDir(), "sso"), Client: sso.Client{HTTP: srv.Client()}}
	p := sso.Provider{Name: "corp", Issuer: srv.URL, ClientID: "c"}
	var polled []string
	old := startSSOPoller
	startSSOPoller = func(root, name string) error { polled = append(polled, root+" "+name); return nil }
	t.Cleanup(func() { startSSOPoller = old })

	for i := range 3 {
		var out bytes.Buffer
		err := ssoToken(ssoTestCmd(), &out, "/repo", st, p)
		if err == nil {
			t.Fatalf("call %d: no error with no login", i)
		}
		msg := err.Error()
		if !strings.Contains(msg, srv.URL+"/activate") || !strings.Contains(msg, "ABCD-EFGH") || !strings.Contains(msg, "saddle sso login corp") {
			t.Errorf("call %d: error = %q", i, msg)
		}
		if strings.Contains(msg, "DEV-SECRET") || out.Len() != 0 {
			t.Errorf("call %d: device code or output leaked: %q %q", i, msg, out.String())
		}
	}
	if n := starts.Load(); n != 1 {
		t.Errorf("device requests = %d, want 1", n)
	}
	if len(polled) != 1 || polled[0] != "/repo corp" {
		t.Errorf("pollers = %v, want one for /repo corp", polled)
	}
}

func TestSSOTokenPrintsCachedToken(t *testing.T) {
	st := sso.Store{Dir: filepath.Join(t.TempDir(), "sso")}
	p := sso.Provider{Name: "corp", Issuer: "https://id.example.com", ClientID: "c"}
	if err := st.Save(p, sso.Tokens{AccessToken: "tok-1", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := ssoToken(ssoTestCmd(), &out, "/repo", st, p); err != nil {
		t.Fatal(err)
	}
	if out.String() != "tok-1\n" {
		t.Fatalf("out = %q, want only the token", out.String())
	}
}

func TestUpSSOLoginWarnsWithoutTerminal(t *testing.T) {
	t.Setenv(sso.DirEnv, t.TempDir())
	cfg := config.Default()
	cfg.SSO = config.SSOMap{"corp": {Issuer: "https://id.example.com", ClientID: "c", Token: "access"}}
	var out bytes.Buffer
	upSSOLogin(ssoTestCmd(), &out, cfg, false)
	if !strings.Contains(out.String(), "SSO corp is not logged in; run saddle sso login corp") {
		t.Fatalf("out = %q", out.String())
	}
	out.Reset()
	upSSOLogin(ssoTestCmd(), &out, config.Default(), true)
	if out.Len() != 0 {
		t.Fatalf("no [sso], but up printed %q", out.String())
	}
}

func TestSSOStatusLineHidesTokens(t *testing.T) {
	now := time.Now()
	line := ssoStatusLine(sso.Status{LoggedIn: true, Refreshable: true, ExpiresAt: now.Add(time.Hour),
		Pending: &sso.DeviceAuth{DeviceCode: "DEV-SECRET", UserCode: "ABCD", VerificationURI: "https://id/activate"}}, now)
	if !strings.Contains(line, "logged in, token expires") || !strings.Contains(line, "(refreshable)") ||
		!strings.Contains(line, "https://id/activate") || strings.Contains(line, "DEV-SECRET") {
		t.Fatalf("line = %q", line)
	}
	if got := ssoStatusLine(sso.Status{}, now); got != "not logged in" {
		t.Fatalf("logged out = %q", got)
	}
}

func TestSSOCommandRegistered(t *testing.T) {
	for _, sub := range []string{"login", "token", "status", "logout"} {
		c, _, err := Root().Find([]string{"sso", sub})
		if err != nil || c.Name() != sub {
			t.Errorf("saddle sso %s: %v", sub, err)
		}
	}
}
