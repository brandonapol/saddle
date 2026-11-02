package app

import (
	"fmt"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/sso"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

// EventSSOLogin is recorded when an SSO login is needed or finishes. Its
// detail names the provider only: never a code, URL or token.
const EventSSOLogin = "sso_login"

// SSOStore is where SSO logins are cached (#758).
func (a *App) SSOStore() (sso.Store, error) {
	dir, err := sso.DefaultDir()
	return sso.Store{Dir: dir}, err
}

// codexSSOArgs are the -c overrides that point codex at the [sso] provider
// feeding it, or nil without one. They name the auth command, never a token.
func (a *App) codexSSOArgs() []string {
	name, s, ok := a.Cfg.SSO.Codex()
	if !ok {
		return nil
	}
	return sso.CodexArgs(a.Bin, a.Root, name, s.CodexProvider, s.BaseURL)
}

// applySSOStatus marks codex usable when its [sso] provider has a login it
// can renew without the owner, whatever codex's own OpenAI login says; and
// not usable, with the sign-in command, when it has none.
func (a *App) applySSOStatus(ss []agent.Status) []agent.Status {
	name, s, ok := a.Cfg.SSO.Codex()
	if !ok {
		return ss
	}
	st := sso.Status{}
	if cache, err := a.SSOStore(); err == nil {
		st, _ = cache.Status(s.Provider(name))
	}
	return ssoAdjust(ss, name, st.LoggedIn)
}

func ssoAdjust(ss []agent.Status, name string, loggedIn bool) []agent.Status {
	for i := range ss {
		if ss[i].Name != usage.Codex || !ss[i].Installed {
			continue
		}
		if loggedIn {
			ss[i].OK, ss[i].Reason = true, ""
		} else {
			ss[i].OK, ss[i].Reason = false, fmt.Sprintf("codex SSO %s is not logged in: run saddle sso login %s", name, name)
		}
	}
	return ss
}

// SSOLoginNeeded tells the orchestrator and the owner that SSO provider
// name needs a sign-in, with the URL and the code to enter. The device code
// itself never leaves the pending file.
func (a *App) SSOLoginNeeded(name string, da sso.DeviceAuth) error {
	a.Store.Event(OrchestratorID, EventSSOLogin, name+": sign-in needed")
	text := fmt.Sprintf("SSO login %s expired, so codex agents can't reach their model. Ask the owner to %s (expires %s). Saddle finishes the login on its own once they approve; agents retry on their next turn.",
		name, da.Prompt(), da.ExpiresAt.Local().Format("15:04"))
	err := a.Notify(OrchestratorID, store.NoticeAction, text)
	if a.OwnerNotify != nil {
		a.OwnerNotify(text)
	} else {
		a.notifyExternal(text) // this process is short-lived: don't detach
	}
	return err
}
