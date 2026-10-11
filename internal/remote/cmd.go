package remote

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/trust"
)

// Command is `saddle remote`. open opens the repo in the working directory.
func Command(open func() (*app.App, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remote",
		Short: "Let other Claude Code sessions see this saddle (opt-in, read-only for now)",
		Long: `Serves a read-only MCP server (status, needs_you) on a loopback address for
Claude Code sessions that aren't the orchestrator: another terminal, or another
machine through ssh -L. Off unless ~/.config/saddle/config.toml has

  [remote]
  enabled = true
  listen = "127.0.0.1:7431"   # loopback only

Every caller needs a token from 'saddle remote token create'. Tokens and the
audit log live in ~/.config/saddle/remote (0600), never in the repo.
See docs/remote-control.md.`,
	}
	cmd.AddCommand(serveCmd(open), stdioCmd(open), tokenCmd(), auditCmd())
	return cmd
}

func loadEnabled() (Config, error) {
	cfg, err := LoadConfig(UserConfigPath())
	if err != nil {
		return cfg, err
	}
	if !cfg.Enabled {
		return cfg, errors.New("remote control is off: set enabled = true under [remote] in " + UserConfigPath())
	}
	return cfg, nil
}

func serveCmd(open func() (*app.App, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Serve the remote MCP endpoint in the foreground",
		Long: `Serves in the foreground. saddle up and the plugin engine already serve it
while they run, when [remote] enabled = true; use this when neither runs.
Only one process serves an address at a time: while this one runs, saddle up
waits and takes over when it stops.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadEnabled()
			if err != nil {
				return err
			}
			if err := cfg.Check(); err != nil {
				return err
			}
			a, err := open()
			if err != nil {
				return err
			}
			defer a.Close()
			h, audit, dir, err := appServer(a, cfg)
			if err != nil {
				return err
			}
			defer func() { _ = audit.Close() }()
			out := cmd.OutOrStdout()
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			s := &Service{Cfg: cfg, Handler: h, Dir: dir, Holder: fmt.Sprintf("saddle remote serve (pid %d) for %s", os.Getpid(), a.Root),
				OnServe: func(addr net.Addr) {
					u := serveURL(cfg, addr)
					fmt.Fprintf(out, "saddle remote: serving %s read-only at %s\n", a.Root, u)
					fmt.Fprintf(out, "add it to a Claude Code session with:\n  claude mcp add --transport http saddle-remote %s --header \"Authorization: Bearer $SADDLE_REMOTE_TOKEN\"\n", u)
					fmt.Fprintf(out, "audit log: %s (saddle remote audit)\n", dir)
				}}
			return s.Run(ctx)
		},
	}
}

func stdioCmd(open func() (*app.App, error)) *cobra.Command {
	var scopes, name, repo string
	cmd := &cobra.Command{
		Use:   "stdio",
		Short: "Serve the remote MCP tools over stdin/stdout, for an ssh forced command",
		Long: `Serves the same scoped tools as serve, over stdin and stdout, with no
listener at all: ssh authenticates, and the forced command pins the scope.
In ~/.ssh/authorized_keys on the host:

  command="saddle remote stdio --scope read --repo /src/saddle --name phone",restrict ssh-ed25519 AAAA…

and on the other machine:

  claude mcp add saddle-box -- ssh box saddle remote stdio

Still off unless [remote] enabled = true. Calls are audited as ssh:NAME.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := loadEnabled(); err != nil {
				return err
			}
			sc, err := ParseScopes(scopes)
			if err != nil {
				return err
			}
			if name != "" && !tokenName.MatchString(name) {
				return fmt.Errorf("name %q: use letters, digits, '.', '_' or '-'", name)
			}
			if repo != "" {
				// A forced command starts in the user's home, not the repo.
				if err := os.Chdir(repo); err != nil {
					return err
				}
			}
			a, err := open()
			if err != nil {
				return err
			}
			defer a.Close()
			if rep, err := trust.Status(a.Root, nil); err != nil || rep.State != trust.StateTrusted {
				return errors.New("this repo isn't trusted: run saddle trust before exposing it")
			}
			cfg, _ := LoadConfig(UserConfigPath())
			dir, err := Dir()
			if err != nil {
				return err
			}
			audit, err := openAppAudit(a, dir, cfg)
			if err != nil {
				return err
			}
			defer func() { _ = audit.Close() }()
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return ServeStdio(ctx, AppSource{A: a}, audit, StdioCaller(sc, name, os.Getenv("SSH_CONNECTION")), &mcp.StdioTransport{})
		},
	}
	cmd.Flags().StringVar(&scopes, "scope", "read", "comma-separated scopes: read, act, land, admin (read is always added)")
	cmd.Flags().StringVar(&name, "name", "", "name for this key in the audit log (ssh:NAME)")
	cmd.Flags().StringVar(&repo, "repo", "", "the repo to serve (a forced command starts in $HOME)")
	return cmd
}

func auditCmd() *cobra.Command {
	var token, tool string
	var denied, asJSON bool
	var since time.Duration
	var limit int
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Show the remote-control audit log (every failed login and every call)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := Dir()
			if err != nil {
				return err
			}
			all, err := ReadAudit(dir)
			if err != nil {
				return err
			}
			var es []AuditEntry
			cut := time.Now().Add(-since)
			for _, e := range all {
				if token != "" && e.Token != token || tool != "" && e.Tool != tool ||
					denied && e.Decision != DecisionDenied || since > 0 && e.TS.Before(cut) {
					continue
				}
				es = append(es, e)
			}
			if limit > 0 && len(es) > limit {
				es = es[len(es)-limit:]
			}
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				for _, e := range es {
					if err := enc.Encode(e); err != nil {
						return err
					}
				}
				return nil
			}
			if len(es) == 0 {
				fmt.Fprintf(out, "no audit entries in %s\n", dir)
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "TIME\tTOKEN\tADDR\tTOOL\tDECISION\tREASON")
			for _, e := range es {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", e.TS.Local().Format(time.DateTime), dash(e.Token), e.Addr, dash(e.Tool), e.Decision, e.Reason)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&token, "token", "", "only this token (or ssh:NAME)")
	cmd.Flags().StringVar(&tool, "tool", "", "only this tool")
	cmd.Flags().BoolVar(&denied, "denied", false, "only refused logins and calls")
	cmd.Flags().DurationVar(&since, "since", 0, "only entries newer than this (e.g. 1h)")
	cmd.Flags().IntVarP(&limit, "limit", "n", 50, "show the last N entries (0 for all)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print entries as JSON lines")
	return cmd
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func tokens() (*Tokens, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	return NewTokens(dir), nil
}

func tokenCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "Create, list and revoke remote-control tokens"}
	var scopes string
	var repos []string
	var ttl time.Duration
	create := &cobra.Command{
		Use:   "create NAME",
		Short: "Issue a token; its secret is printed once and never stored",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sc, err := ParseScopes(scopes)
			if err != nil {
				return err
			}
			ts, err := tokens()
			if err != nil {
				return err
			}
			secret, err := ts.Create(args[0], sc, ttl, repos...)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), secret)
			fmt.Fprintf(cmd.ErrOrStderr(), "token %q (%s, repos %s) expires in %v; this is the only time the secret is shown\n", args[0], joinScopes(sc), joinRepos(repos), ttl)
			return nil
		},
	}
	create.Flags().StringVar(&scopes, "scope", "read", "comma-separated scopes: read, act, land, admin (read is always added)")
	create.Flags().StringSliceVar(&repos, "repo", nil, "limit the token to this repo (a name or absolute path; repeat for more; default any)")
	create.Flags().DurationVar(&ttl, "ttl", DefaultTTL, fmt.Sprintf("how long the token lives (at most %v)", MaxTTL))

	list := &cobra.Command{
		Use:   "list",
		Short: "List tokens (names, scopes and expiry; never secrets)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ts, err := tokens()
			if err != nil {
				return err
			}
			toks, err := ts.List()
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSCOPES\tREPOS\tEXPIRES\tSTATE")
			now := time.Now()
			for _, t := range toks {
				state := "live"
				switch {
				case t.Revoked != nil:
					state = "revoked"
				case !now.Before(t.Expires):
					state = "expired"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.Name, joinScopes(t.Scopes), joinRepos(t.Repos), t.Expires.Local().Format(time.DateTime), state)
			}
			return tw.Flush()
		},
	}
	revoke := &cobra.Command{
		Use:   "revoke NAME",
		Short: "Revoke a token; a running server refuses it on the next request",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ts, err := tokens()
			if err != nil {
				return err
			}
			if err := ts.Revoke(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "revoked %s\n", args[0])
			return nil
		},
	}
	cmd.AddCommand(create, list, revoke)
	return cmd
}

func joinScopes(sc []Scope) string {
	s := make([]string, len(sc))
	for i, x := range sc {
		s[i] = string(x)
	}
	return strings.Join(s, ",")
}

func joinRepos(rs []string) string {
	if len(rs) == 0 {
		return "any"
	}
	return strings.Join(rs, ",")
}
