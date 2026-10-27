package sso

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeIdP is an OIDC provider with discovery, the device grant and
// refresh tokens that rotate on every use, like Keycloak's and Okta's.
type fakeIdP struct {
	srv *httptest.Server
	mu  sync.Mutex
	// devicePolls are the error codes the device grant answers with, in
	// order, before it succeeds.
	devicePolls   []string
	deviceStarts  atomic.Int32
	refreshes     atomic.Int32
	refreshDelay  time.Duration
	refreshError  string // answer refresh_token grants with this error
	expiresIn     int
	issued        int
	validRefresh  map[string]bool
	noDevice      bool
	lastAudience  string
	refreshExpiry int
}

const fakeDeviceCode = "DEVICE-CODE-SECRET"

func newFakeIdP(t *testing.T) *fakeIdP {
	f := &fakeIdP{expiresIn: 300, validRefresh: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/realms/eng/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		doc := map[string]string{"issuer": f.srv.URL + "/realms/eng", "token_endpoint": f.srv.URL + "/token"}
		if !f.noDevice {
			doc["device_authorization_endpoint"] = f.srv.URL + "/device"
		}
		_ = json.NewEncoder(w).Encode(doc)
	})
	mux.HandleFunc("/device", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("client_id") != "saddle-cli" {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		f.deviceStarts.Add(1)
		f.mu.Lock()
		f.lastAudience = r.PostForm.Get("audience")
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code": fakeDeviceCode, "user_code": "WDJB-MJHT",
			"verification_uri":          f.srv.URL + "/activate",
			"verification_uri_complete": f.srv.URL + "/activate?user_code=WDJB-MJHT",
			"expires_in":                600, "interval": 1,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		fail := func(code string) {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": "Token is not active"})
		}
		switch r.PostForm.Get("grant_type") {
		case "urn:ietf:params:oauth:grant-type:device_code":
			if r.PostForm.Get("device_code") != fakeDeviceCode {
				fail("invalid_grant")
				return
			}
			if len(f.devicePolls) > 0 {
				code := f.devicePolls[0]
				f.devicePolls = f.devicePolls[1:]
				fail(code)
				return
			}
		case "refresh_token":
			f.refreshes.Add(1)
			time.Sleep(f.refreshDelay)
			if f.refreshError != "" {
				fail(f.refreshError)
				return
			}
			rt := r.PostForm.Get("refresh_token")
			if !f.validRefresh[rt] {
				fail("invalid_grant") // reused or revoked
				return
			}
			delete(f.validRefresh, rt) // rotation
		default:
			fail("unsupported_grant_type")
			return
		}
		f.issued++
		rt := fmt.Sprintf("refresh-%d", f.issued)
		f.validRefresh[rt] = true
		body := map[string]any{
			"access_token": fmt.Sprintf("access-%d", f.issued), "id_token": fmt.Sprintf("id-%d", f.issued),
			"refresh_token": rt, "expires_in": f.expiresIn, "token_type": "Bearer",
		}
		if f.refreshExpiry > 0 {
			body["refresh_expires_in"] = f.refreshExpiry
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIdP) provider() Provider {
	return Provider{Name: "corp", Issuer: f.srv.URL + "/realms/eng", ClientID: "saddle-cli"}
}

// clock is a settable time source.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newStore(t *testing.T, f *fakeIdP) (Store, *clock) {
	c := &clock{now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	cl := Client{HTTP: f.srv.Client(), Now: c.Now, Sleep: func(context.Context, time.Duration) error { return nil }}
	return Store{Dir: filepath.Join(t.TempDir(), "sso"), Client: cl}, c
}

func TestDeviceLoginPollsThenCaches(t *testing.T) {
	f := newFakeIdP(t)
	f.devicePolls = []string{"authorization_pending", "slow_down", "authorization_pending"}
	st, _ := newStore(t, f)
	p := f.provider()
	var shown DeviceAuth
	if err := st.Login(context.Background(), p, func(da DeviceAuth) { shown = da }); err != nil {
		t.Fatal(err)
	}
	if shown.UserCode != "WDJB-MJHT" || !strings.Contains(shown.Prompt(), "activate?user_code=WDJB-MJHT") {
		t.Fatalf("prompt = %q", shown.Prompt())
	}
	if strings.Contains(shown.Prompt(), fakeDeviceCode) {
		t.Fatal("prompt shows the device code")
	}
	fi, err := os.Stat(st.path(p, ".json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("cache mode = %v, want 0600", fi.Mode().Perm())
	}
	di, _ := os.Stat(st.Dir)
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, want 0700", di.Mode().Perm())
	}
	tok, err := st.Token(context.Background(), p)
	if err != nil || tok != "access-1" {
		t.Fatalf("token = %q, %v", tok, err)
	}
	if n := f.refreshes.Load(); n != 0 {
		t.Fatalf("a valid cached token refreshed %d times", n)
	}
}

func TestSlowDownBacksOff(t *testing.T) {
	f := newFakeIdP(t)
	f.devicePolls = []string{"slow_down", "slow_down"}
	st, _ := newStore(t, f)
	var waits []time.Duration
	st.Client.Sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	if err := st.Login(context.Background(), f.provider(), func(DeviceAuth) {}); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{time.Second, 6 * time.Second, 11 * time.Second}
	if fmt.Sprint(waits) != fmt.Sprint(want) {
		t.Fatalf("waits = %v, want %v", waits, want)
	}
}

func TestDeviceLoginDeniedOrExpired(t *testing.T) {
	for code, want := range map[string]string{"access_denied": "denied", "expired_token": "expired"} {
		t.Run(code, func(t *testing.T) {
			f := newFakeIdP(t)
			f.devicePolls = []string{"authorization_pending", code}
			st, _ := newStore(t, f)
			err := st.Login(context.Background(), f.provider(), func(DeviceAuth) {})
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v, want %q", err, want)
			}
			if _, ok, _ := st.Load(f.provider()); ok {
				t.Fatal("a failed login cached tokens")
			}
		})
	}
}

func TestNoDeviceEndpoint(t *testing.T) {
	f := newFakeIdP(t)
	f.noDevice = true
	st, _ := newStore(t, f)
	err := st.Login(context.Background(), f.provider(), func(DeviceAuth) {})
	if err == nil || !strings.Contains(err.Error(), "device authorization grant") {
		t.Fatalf("err = %v", err)
	}
}

func TestTokenRefreshesNearExpiryAndSavesRotation(t *testing.T) {
	f := newFakeIdP(t)
	st, clk := newStore(t, f)
	p := f.provider()
	if err := st.Login(context.Background(), p, func(DeviceAuth) {}); err != nil {
		t.Fatal(err)
	}
	clk.Add(250 * time.Second) // 50s left: inside Skew
	tok, err := st.Token(context.Background(), p)
	if err != nil || tok != "access-2" {
		t.Fatalf("token = %q, %v", tok, err)
	}
	cached, _, _ := st.Load(p)
	if cached.RefreshToken != "refresh-2" {
		t.Fatalf("rotated refresh token not saved: %q", cached.RefreshToken)
	}
	clk.Add(250 * time.Second)
	if tok, err := st.Token(context.Background(), p); err != nil || tok != "access-3" {
		t.Fatalf("second refresh = %q, %v", tok, err)
	}
}

func TestTokenIDKind(t *testing.T) {
	f := newFakeIdP(t)
	st, _ := newStore(t, f)
	p := f.provider()
	p.Token = TokenID
	if err := st.Login(context.Background(), p, func(DeviceAuth) {}); err != nil {
		t.Fatal(err)
	}
	if tok, err := st.Token(context.Background(), p); err != nil || tok != "id-1" {
		t.Fatalf("token = %q, %v", tok, err)
	}
}

func TestConcurrentTokensRefreshOnce(t *testing.T) {
	f := newFakeIdP(t)
	f.refreshDelay = 20 * time.Millisecond
	st, clk := newStore(t, f)
	p := f.provider()
	if err := st.Login(context.Background(), p, func(DeviceAuth) {}); err != nil {
		t.Fatal(err)
	}
	clk.Add(280 * time.Second)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			tok, err := st.Token(context.Background(), p)
			if err == nil && tok != "access-2" {
				err = fmt.Errorf("token %q", tok)
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := f.refreshes.Load(); n != 1 {
		t.Fatalf("refreshed %d times, want 1 (a reused rotating refresh token gets revoked)", n)
	}
}

func TestTokenLoginRequired(t *testing.T) {
	f := newFakeIdP(t)
	st, clk := newStore(t, f)
	p := f.provider()
	if _, err := st.Token(context.Background(), p); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("no cache: err = %v", err)
	}
	if err := st.Login(context.Background(), p, func(DeviceAuth) {}); err != nil {
		t.Fatal(err)
	}
	f.refreshError = "invalid_grant"
	clk.Add(time.Hour)
	_, err := st.Token(context.Background(), p)
	if !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("revoked refresh: err = %v", err)
	}
	if strings.Contains(err.Error(), "refresh-1") || strings.Contains(err.Error(), "access-1") {
		t.Fatalf("error carries a token: %v", err)
	}
	if _, ok, _ := st.Load(p); ok {
		t.Fatal("a dead refresh token stayed cached")
	}
}

func TestRefreshExpiryKnownSkipsNetwork(t *testing.T) {
	f := newFakeIdP(t)
	f.refreshExpiry = 1800
	st, clk := newStore(t, f)
	p := f.provider()
	if err := st.Login(context.Background(), p, func(DeviceAuth) {}); err != nil {
		t.Fatal(err)
	}
	clk.Add(time.Hour)
	if s, _ := st.Status(p); s.LoggedIn {
		t.Fatal("status says logged in past the refresh token's expiry")
	}
	if _, err := st.Token(context.Background(), p); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("err = %v", err)
	}
	if n := f.refreshes.Load(); n != 0 {
		t.Fatalf("tried an expired refresh token %d times", n)
	}
}

func TestStartPendingOnceThenFinish(t *testing.T) {
	f := newFakeIdP(t)
	st, _ := newStore(t, f)
	p := f.provider()
	p.Audience = "api://gateway"
	da, fresh, err := st.StartPending(context.Background(), p)
	if err != nil || !fresh {
		t.Fatalf("first: fresh=%v err=%v", fresh, err)
	}
	da2, fresh, err := st.StartPending(context.Background(), p)
	if err != nil || fresh || da2.UserCode != da.UserCode {
		t.Fatalf("second: fresh=%v err=%v", fresh, err)
	}
	if n := f.deviceStarts.Load(); n != 1 {
		t.Fatalf("device requests = %d, want 1", n)
	}
	if f.lastAudience != "api://gateway" {
		t.Fatalf("audience = %q", f.lastAudience)
	}
	fi, _ := os.Stat(st.path(p, ".pending.json"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("pending mode = %v", fi.Mode().Perm())
	}
	if s, _ := st.Status(p); s.Pending == nil || s.LoggedIn {
		t.Fatalf("status = %+v", s)
	}
	if err := st.FinishPending(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	s, _ := st.Status(p)
	if !s.LoggedIn || s.Pending != nil {
		t.Fatalf("after finish: %+v", s)
	}
	if err := st.Logout(p); err != nil {
		t.Fatal(err)
	}
	if s, _ := st.Status(p); s.LoggedIn {
		t.Fatal("still logged in after logout")
	}
}

func TestPendingExpiresStartsNew(t *testing.T) {
	f := newFakeIdP(t)
	st, clk := newStore(t, f)
	p := f.provider()
	if _, _, err := st.StartPending(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	clk.Add(11 * time.Minute)
	if _, fresh, err := st.StartPending(context.Background(), p); err != nil || !fresh {
		t.Fatalf("expired pending sign-in reused: fresh=%v err=%v", fresh, err)
	}
}

func TestNonJSONErrorBodyIsDropped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "openid-configuration") {
			_ = json.NewEncoder(w).Encode(map[string]string{"token_endpoint": "http://" + r.Host + "/token"})
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>proxy echoed refresh_token=SECRET-RT</html>"))
	}))
	defer srv.Close()
	c := Client{HTTP: srv.Client()}
	_, err := c.Refresh(context.Background(), Provider{Name: "x", Issuer: srv.URL, ClientID: "c"}, "SECRET-RT")
	if err == nil || strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), "http 502") {
		t.Fatalf("err = %v", err)
	}
	if IsInvalidGrant(err) {
		t.Fatal("a gateway error counted as a dead refresh token")
	}
}

func TestCheckIssuer(t *testing.T) {
	for issuer, ok := range map[string]bool{
		"https://id.example.com/realms/eng":     true,
		"https://dev-1.okta.com/oauth2/default": true,
		"http://127.0.0.1:8080/realms/x":        true,
		"http://localhost:8080":                 true,
		"http://id.example.com":                 false,
		"id.example.com":                        false,
		"":                                      false,
	} {
		if err := CheckIssuer(issuer); (err == nil) != ok {
			t.Errorf("CheckIssuer(%q) = %v, want ok=%v", issuer, err, ok)
		}
	}
}

func TestJWTExpWhenNoExpiresIn(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1793000000}`))
	exp, ok := jwtExp("h." + payload + ".s")
	if !ok || exp.Unix() != 1793000000 {
		t.Fatalf("exp = %v, %v", exp, ok)
	}
	if _, ok := jwtExp("opaque"); ok {
		t.Fatal("opaque token parsed")
	}
}

func TestCodexArgs(t *testing.T) {
	got := strings.Join(CodexArgs("/usr/bin/saddle", "/repo", "corp", "corp-gw", ""), " ")
	for _, want := range []string{
		`-c model_provider="corp-gw"`,
		`-c model_providers.corp-gw.auth.command="/usr/bin/saddle"`,
		`-c model_providers.corp-gw.auth.args=["sso","token","--root","/repo","corp"]`,
		`model_providers.corp-gw.auth.timeout_ms=15000`,
		`model_providers.corp-gw.auth.refresh_interval_ms=60000`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("args lack %s:\n%s", want, got)
		}
	}
	if strings.Contains(got, "base_url") {
		t.Errorf("no base_url configured, but args set one: %s", got)
	}
	got = strings.Join(CodexArgs("/b", "/r", "corp", "my.gw", "https://gw.example.com/v1"), " ")
	for _, want := range []string{`model_providers."my.gw".base_url="https://gw.example.com/v1"`, `model_providers."my.gw".name="my.gw"`} {
		if !strings.Contains(got, want) {
			t.Errorf("args lack %s:\n%s", want, got)
		}
	}
}
