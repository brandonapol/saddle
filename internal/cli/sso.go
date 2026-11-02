package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/sso"
	"github.com/spf13/cobra"
)

// ssoCmd is saddle sso: one OIDC login shared by every codex agent (#758).
func ssoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sso",
		Short: "Log in once to an OIDC provider (Keycloak, Okta) whose tokens feed codex agents",
		Long: `Configure an OIDC provider in [sso.<name>] (see docs/sso.md) and log in
once; codex workers and a codex orchestrator then get a fresh bearer token
from saddle for the codex model provider it names. Tokens are cached per
user, mode 0600, and never written to launch files, env vars or briefs.

  saddle sso login [name]   sign in with the device flow (URL + code)
  saddle sso status         who is logged in and until when; no tokens shown
  saddle sso logout [name]  forget a login
  saddle sso token <name>   print a token (codex runs this; you don't need to)`,
	}
	cmd.AddCommand(ssoLoginCmd(), ssoTokenCmd(), ssoStatusCmd(), ssoLogoutCmd())
	return cmd
}

// ssoConfig loads the config of root: the flag, else $SADDLE_ROOT, else the
// repo around the working directory.
func ssoConfig(root string) (string, config.Config, error) {
	if root == "" {
		root = os.Getenv("SADDLE_ROOT")
	}
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", config.Config{}, err
		}
		if root, err = gitx.Root(wd); err != nil {
			return "", config.Config{}, fmt.Errorf("not in a git repo (pass --root): %w", err)
		}
	}
	cfg, err := config.Load(root)
	return root, cfg, err
}

// ssoProviders resolves the names given (all configured ones when none).
func ssoProviders(cfg config.Config, names []string) ([]sso.Provider, error) {
	if len(cfg.SSO) == 0 {
		return nil, errors.New("no [sso.<name>] provider is configured; see docs/sso.md")
	}
	if len(names) == 0 {
		names = cfg.SSO.Names()
	}
	var out []sso.Provider
	for _, n := range names {
		s, ok := cfg.SSO[n]
		if !ok {
			return nil, fmt.Errorf("no [sso.%s] provider; configured: %v", n, cfg.SSO.Names())
		}
		out = append(out, s.Provider(n))
	}
	return out, nil
}

func ssoStore() (sso.Store, error) {
	dir, err := sso.DefaultDir()
	return sso.Store{Dir: dir}, err
}

func ssoLoginCmd() *cobra.Command {
	var root string
	var pending bool
	cmd := &cobra.Command{
		Use:   "login [name]",
		Short: "Sign in with the OIDC device flow and cache the tokens",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, cfg, err := ssoConfig(root)
			if err != nil {
				return err
			}
			ps, err := ssoProviders(cfg, args)
			if err != nil {
				return err
			}
			st, err := ssoStore()
			if err != nil {
				return err
			}
			if pending {
				if len(ps) != 1 {
					return errors.New("--pending takes one provider name")
				}
				return finishPendingLogin(cmd, root, st, ps[0])
			}
			for _, p := range ps {
				if err := ssoLogin(cmd, cmd.OutOrStdout(), st, p); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&root, "root", "", "repo whose config names the provider (default: the current repo)")
	cmd.Flags().BoolVar(&pending, "pending", false, "finish the sign-in saddle sso token started, telling the orchestrator (internal)")
	_ = cmd.Flags().MarkHidden("pending")
	return cmd
}

// ssoLogin runs the device flow in the foreground, printing the URL and code.
func ssoLogin(cmd *cobra.Command, out io.Writer, st sso.Store, p sso.Provider) error {
	err := st.Login(cmd.Context(), p, func(da sso.DeviceAuth) {
		fmt.Fprintf(out, "SSO %s: to sign in, %s\nWaiting for approval (expires %s)...\n", p.Name, da.Prompt(), da.ExpiresAt.Local().Format("15:04"))
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "SSO %s: logged in.\n", p.Name)
	return nil
}

// finishPendingLogin is the detached poller saddle sso token starts: it
// tells the orchestrator and the owner where to sign in, then waits for
// the approval and caches the tokens.
func finishPendingLogin(cmd *cobra.Command, root string, st sso.Store, p sso.Provider) error {
	s, err := st.Status(p)
	if err != nil {
		return err
	}
	if s.Pending == nil {
		return fmt.Errorf("sso %s: no sign-in is pending", p.Name)
	}
	a, err := app.Open(root)
	if err == nil {
		defer a.Close()
		_ = a.SSOLoginNeeded(p.Name, *s.Pending)
	}
	if err := st.FinishPending(cmd.Context(), p); err != nil {
		if a != nil {
			a.Store.Event(app.OrchestratorID, app.EventSSOLogin, p.Name+": sign-in failed")
		}
		return err
	}
	if a != nil {
		a.Store.Event(app.OrchestratorID, app.EventSSOLogin, p.Name+": logged in")
	}
	return nil
}

// startSSOPoller starts `saddle sso login --pending` detached from the
// agent that asked for a token, so the sign-in finishes after it exits.
var startSSOPoller = func(root, name string) error {
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	c := exec.Command(bin, "sso", "login", "--pending", "--root", root, name)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	c.Dir = root
	if err := c.Start(); err != nil {
		return err
	}
	return c.Process.Release()
}

func ssoTokenCmd() *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:   "token <name>",
		Short: "Print a valid token for an SSO provider (the codex auth command)",
		Long: `Prints a bearer token good for at least a minute, refreshing it when needed.
Codex runs this as [model_providers.<id>.auth] command. When no login can
be renewed it starts one sign-in, tells the orchestrator the URL and code,
and exits non-zero; agents retry once the owner approves.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, cfg, err := ssoConfig(root)
			if err != nil {
				return err
			}
			ps, err := ssoProviders(cfg, args)
			if err != nil {
				return err
			}
			st, err := ssoStore()
			if err != nil {
				return err
			}
			return ssoToken(cmd, cmd.OutOrStdout(), root, st, ps[0])
		},
	}
	cmd.Flags().StringVar(&root, "root", "", "repo whose config names the provider (default: the current repo)")
	return cmd
}

// ssoToken prints p's token, or starts the background sign-in once and
// returns an error naming where to sign in.
func ssoToken(cmd *cobra.Command, out io.Writer, root string, st sso.Store, p sso.Provider) error {
	tok, err := st.Token(cmd.Context(), p)
	if err == nil {
		_, err = fmt.Fprintln(out, tok)
		return err
	}
	if !errors.Is(err, sso.ErrLoginRequired) {
		return err
	}
	da, fresh, serr := st.StartPending(cmd.Context(), p)
	if serr != nil {
		return fmt.Errorf("sso %s: login needed (run saddle sso login %s): %v", p.Name, p.Name, serr)
	}
	if fresh {
		if perr := startSSOPoller(root, p.Name); perr != nil {
			return fmt.Errorf("sso %s: login needed: run saddle sso login %s (%v)", p.Name, p.Name, perr)
		}
	}
	return fmt.Errorf("sso %s: login needed: %s, or run saddle sso login %s", p.Name, da.Prompt(), p.Name)
}

func ssoStatusCmd() *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show each SSO provider's login, without tokens",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, cfg, err := ssoConfig(root)
			if err != nil {
				return err
			}
			ps, err := ssoProviders(cfg, nil)
			if err != nil {
				return err
			}
			st, err := ssoStore()
			if err != nil {
				return err
			}
			for _, p := range ps {
				s, err := st.Status(p)
				if err != nil {
					return err
				}
				feeds := ""
				if c := cfg.SSO[p.Name].CodexProvider; c != "" {
					feeds = " -> codex provider " + c
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s (%s%s): %s\n", p.Name, p.Issuer, feeds, ssoStatusLine(s, time.Now()))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&root, "root", "", "repo whose config names the providers (default: the current repo)")
	return cmd
}

func ssoStatusLine(s sso.Status, now time.Time) string {
	var line string
	switch {
	case !s.LoggedIn:
		line = "not logged in"
	case now.Before(s.ExpiresAt):
		line = "logged in, token expires " + s.ExpiresAt.Local().Format("Jan 2 15:04")
	default:
		line = "logged in, token renews on next use"
	}
	if s.LoggedIn && s.Refreshable {
		line += " (refreshable)"
	}
	if s.Pending != nil {
		line += "; sign-in pending: " + s.Pending.Prompt()
	}
	return line
}

func ssoLogoutCmd() *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:   "logout [name]",
		Short: "Forget an SSO login (all when no name is given)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, cfg, err := ssoConfig(root)
			if err != nil {
				return err
			}
			ps, err := ssoProviders(cfg, args)
			if err != nil {
				return err
			}
			st, err := ssoStore()
			if err != nil {
				return err
			}
			for _, p := range ps {
				if err := st.Logout(p); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "SSO %s: logged out.\n", p.Name)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&root, "root", "", "repo whose config names the providers (default: the current repo)")
	return cmd
}

// upSSOLogin is saddle up's one-time sign-in: each [sso] provider without a
// usable login runs the device flow before the TUI opens, when up has a
// terminal. Without one, or when sign-in fails, it warns and up goes on:
// codex agents will ask for the sign-in through the orchestrator instead.
func upSSOLogin(cmd *cobra.Command, out io.Writer, cfg config.Config, interactive bool) {
	if len(cfg.SSO) == 0 {
		return
	}
	st, err := ssoStore()
	if err != nil {
		fmt.Fprintln(out, "warning: sso: "+err.Error())
		return
	}
	ps, _ := ssoProviders(cfg, nil)
	for _, p := range ps {
		s, err := st.Status(p)
		if err == nil && s.LoggedIn {
			continue
		}
		if !interactive {
			fmt.Fprintf(out, "warning: SSO %s is not logged in; run saddle sso login %s\n", p.Name, p.Name)
			continue
		}
		if err := ssoLogin(cmd, out, st, p); err != nil {
			fmt.Fprintf(out, "warning: SSO %s login failed: %v; codex agents will ask for it through the orchestrator\n", p.Name, err)
		}
	}
}

// isTerminal reports whether r is a terminal.
func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
