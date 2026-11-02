package app

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/sso"
	"github.com/brandonapol/saddle/internal/usage"
)

func ssoCfg() config.Config {
	cfg := config.Default()
	cfg.Adapters = config.AdapterMap{usage.Codex: {Cmd: "codex", Args: []string{"-c", "foo=1"}}}
	cfg.SSO = config.SSOMap{"corp": {Issuer: "https://id.example.com/realms/eng", ClientID: "saddle-cli", Token: "access", CodexProvider: "corp-gw"}}
	return cfg
}

// #758: codex launches, workers and the orchestrator alike, name saddle's
// token command for the SSO provider; the user's own args still come last.
func TestCodexAdapterCmdCarriesSSOAuth(t *testing.T) {
	a := &App{Root: "/repo", Bin: "/bin/saddle", Cfg: ssoCfg()}
	cmd, args := a.adapterCmd(usage.Codex)
	got := strings.Join(args, " ")
	if cmd != "codex" {
		t.Fatalf("cmd = %q", cmd)
	}
	for _, want := range []string{`model_provider="corp-gw"`, `model_providers.corp-gw.auth.command="/bin/saddle"`,
		`auth.args=["sso","token","--root","/repo","corp"]`} {
		if !strings.Contains(got, want) {
			t.Errorf("args lack %s: %s", want, got)
		}
	}
	if !strings.HasSuffix(got, "-c foo=1") {
		t.Errorf("user args should come last to win: %s", got)
	}
	// Other adapters are untouched.
	if _, gargs := a.adapterCmd(usage.Grok); len(gargs) != 0 {
		t.Errorf("grok args = %v", gargs)
	}
}

func TestCodexAdapterCmdWithoutSSOUnchanged(t *testing.T) {
	cfg := ssoCfg()
	cfg.SSO = nil
	a := &App{Root: "/repo", Bin: "/bin/saddle", Cfg: cfg}
	if _, args := a.adapterCmd(usage.Codex); strings.Join(args, " ") != "-c foo=1" {
		t.Fatalf("args = %v", args)
	}
	if a.codexSSOArgs() != nil {
		t.Fatal("no [sso], but SSO args")
	}
}

func TestSSOAdjustAvailability(t *testing.T) {
	base := func() []agent.Status {
		return []agent.Status{
			{Name: "claude", OK: true, Installed: true},
			{Name: "codex", OK: false, Installed: true, Reason: "codex is not logged in: run codex login"},
			{Name: "grok", OK: false, Installed: false, Reason: "grok not on PATH"},
		}
	}
	ss := ssoAdjust(base(), "corp", true)
	if !ss[1].OK || ss[1].Reason != "" {
		t.Fatalf("codex with an SSO login: %+v", ss[1])
	}
	ss = ssoAdjust(base(), "corp", false)
	if ss[1].OK || !strings.Contains(ss[1].Reason, "saddle sso login corp") {
		t.Fatalf("codex without an SSO login: %+v", ss[1])
	}
	missing := base()
	missing[1].Installed = false
	missing[1].Reason = "codex not on PATH"
	if ss := ssoAdjust(missing, "corp", true); ss[1].OK {
		t.Fatal("codex not on PATH counted as usable")
	}
	if ss := ssoAdjust(base(), "corp", true); ss[2].OK || !ss[0].OK {
		t.Fatalf("other adapters changed: %+v", ss)
	}
}

func TestApplySSOStatusReadsCache(t *testing.T) {
	t.Setenv(sso.DirEnv, t.TempDir())
	a := &App{Root: "/repo", Bin: "/bin/saddle", Cfg: ssoCfg()}
	in := []agent.Status{{Name: "codex", OK: true, Installed: true}}
	if ss := a.applySSOStatus(in); ss[0].OK {
		t.Fatal("no cached SSO login, but codex is usable")
	}
}
