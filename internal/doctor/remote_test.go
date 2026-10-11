package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/remote"
)

// remoteEnv adds a remote-control report to the fake.
type remoteEnv struct {
	*fakeEnv
	rep RemoteReport
}

func (e remoteEnv) Remote() (RemoteReport, error) { return e.rep, nil }

func remoteResult(t *testing.T, env Env) Result {
	t.Helper()
	for _, r := range Run(env) {
		if r.Name == CheckRemoteControl {
			return r
		}
	}
	t.Fatal("no remote control check")
	return Result{}
}

// #315: the check says what is exposed, and warns on loose modes and long
// token lives.
func TestRemoteControlCheck(t *testing.T) {
	base := healthy(t)
	now := time.Now()
	tok := func(name string, life time.Duration, scopes ...remote.Scope) remote.Token {
		return remote.Token{Name: name, Scopes: scopes, Created: now.Add(-time.Hour), Expires: now.Add(-time.Hour + life)}
	}
	revokedAt := now.Add(-time.Minute)
	revoked := tok("gone", 24*time.Hour, remote.ScopeAdmin)
	revoked.Revoked = &revokedAt
	on := remote.Config{Enabled: true, Listen: "127.0.0.1:7777"}
	pki := t.TempDir()
	cert, key := filepath.Join(pki, "c.pem"), filepath.Join(pki, "k.pem")
	for _, p := range []string{cert, key} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name   string
		rep    RemoteReport
		status Status
		detail []string
		fix    string
	}{
		{"off", RemoteReport{Config: remote.Config{Listen: "127.0.0.1:7777"}}, OK, []string{"off", "no live tokens"}, ""},
		{"on with tokens", RemoteReport{Config: on, Tokens: []remote.Token{
			tok("phone", 24*time.Hour, remote.ScopeRead, remote.ScopeAct),
			tok("old", 30*time.Minute, remote.ScopeAdmin), // expired
			revoked,
		}}, OK, []string{"on, listening on 127.0.0.1:7777 (loopback, plain HTTP)", "1 live token: phone (read,act"}, ""},
		{"public TLS", RemoteReport{Config: remote.Config{Enabled: true, Listen: "100.64.0.1:7777", AllowPublic: true, TLSCert: cert, TLSKey: key}},
			OK, []string{"100.64.0.1:7777 (public, TLS)"}, ""},
		{"long ttl", RemoteReport{Config: on, Tokens: []remote.Token{tok("laptop", 20*24*time.Hour, remote.ScopeLand)}},
			Warn, []string{"laptop lives 20d"}, "saddle remote token revoke laptop"},
		{"loose modes", RemoteReport{Config: on, Dir: "/r", Loose: []string{"/r (mode -rwxr-xr-x)", "/r/tokens.json (mode -rw-r--r--)"}},
			Warn, []string{"/r/tokens.json (mode -rw-r--r--)"}, "chmod"},
		{"bad config", RemoteReport{ConfigErr: errors.New("toml: bad")}, Fail, []string{"toml: bad"}, "[remote]"},
		{"refused bind", RemoteReport{Config: remote.Config{Enabled: true, Listen: "0.0.0.0:7777"}}, Fail, []string{"0.0.0.0:7777"}, "[remote]"},
		{"tokens unreadable", RemoteReport{Config: on, TokensErr: errors.New("boom")}, Warn, []string{"boom"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := remoteResult(t, remoteEnv{fakeEnv: base, rep: tc.rep})
			if r.Status != tc.status || !strings.Contains(r.Fix, tc.fix) {
				t.Fatalf("%+v", r)
			}
			for _, d := range tc.detail {
				if !strings.Contains(r.Detail, d) {
					t.Errorf("detail %q lacks %q", r.Detail, d)
				}
			}
			if r.About == "" {
				t.Error("no about text")
			}
		})
	}
}

// ReadRemote finds loose modes on the remote dir and its files, and lists
// the tokens when their file is private.
func TestReadRemoteReport(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "remote")
	if _, err := remote.NewTokens(dir).Create("phone", []remote.Scope{remote.ScopeRead}, time.Hour); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg, []byte("[remote]\nenabled = true\nlisten = \"127.0.0.1:7788\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep := ReadRemote(cfg, dir)
	if rep.ConfigErr != nil || !rep.Config.Enabled || rep.Config.Listen != "127.0.0.1:7788" {
		t.Fatalf("config = %+v, %v", rep.Config, rep.ConfigErr)
	}
	if len(rep.Tokens) != 1 || rep.Tokens[0].Name != "phone" || len(rep.Loose) != 0 {
		t.Fatalf("report = %+v", rep)
	}

	if err := os.WriteFile(filepath.Join(dir, "audit.log"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "tokens.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rep = ReadRemote(cfg, dir)
	got := strings.Join(rep.Loose, "\n")
	for _, p := range []string{dir + " ", filepath.Join(dir, "tokens.json"), filepath.Join(dir, "audit.log")} {
		if !strings.Contains(got, p) {
			t.Errorf("loose = %q, want %s", got, p)
		}
	}
	if rep.TokensErr == nil {
		t.Error("a tokens file others can read listed fine; want the refusal reported")
	}

	if rep := ReadRemote(cfg, filepath.Join(t.TempDir(), "none")); len(rep.Loose) != 0 || rep.TokensErr != nil || len(rep.Tokens) != 0 {
		t.Fatalf("missing dir = %+v, want nothing to report", rep)
	}
}
