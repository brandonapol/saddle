package remote

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

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
	cmd.AddCommand(serveCmd(open), tokenCmd())
	return cmd
}

func serveCmd(open func() (*app.App, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Serve the remote MCP endpoint in the foreground",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := LoadConfig(UserConfigPath())
			if err != nil {
				return err
			}
			if !cfg.Enabled {
				return errors.New("remote control is off: set enabled = true under [remote] in " + UserConfigPath())
			}
			if err := CheckListen(cfg.Listen); err != nil {
				return err
			}
			a, err := open()
			if err != nil {
				return err
			}
			defer a.Close()
			if rep, err := trust.Status(a.Root, nil); err != nil || rep.State != trust.StateTrusted {
				return errors.New("this repo isn't trusted: run saddle trust before exposing it")
			}
			dir, err := Dir()
			if err != nil {
				return err
			}
			audit, err := OpenAudit(dir)
			if err != nil {
				return err
			}
			defer func() { _ = audit.Close() }()
			audit.Mirror = func(e AuditEntry) {
				a.Store.Event(app.OrchestratorID, "remote", fmt.Sprintf("%s %s %s from %s %s", e.Token, e.Decision, e.Tool, e.Addr, e.Reason))
			}
			ln, err := net.Listen("tcp", cfg.Listen)
			if err != nil {
				return err
			}
			srv := &http.Server{Handler: Handler(AppSource{A: a}, NewTokens(dir), audit, cfg.Limits()), ReadHeaderTimeout: 10 * time.Second}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "saddle remote: serving %s read-only at http://%s%s\n", a.Root, ln.Addr(), MCPPath)
			fmt.Fprintf(out, "add it to a Claude Code session with:\n  claude mcp add --transport http saddle-remote http://%s%s --header \"Authorization: Bearer $SADDLE_REMOTE_TOKEN\"\n", ln.Addr(), MCPPath)
			fmt.Fprintf(out, "audit log: %s\n", dir)
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			go func() {
				<-ctx.Done()
				sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = srv.Shutdown(sctx)
			}()
			if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}
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
			secret, err := ts.Create(args[0], sc, ttl)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), secret)
			fmt.Fprintf(cmd.ErrOrStderr(), "token %q (%s) expires in %v; this is the only time the secret is shown\n", args[0], joinScopes(sc), ttl)
			return nil
		},
	}
	create.Flags().StringVar(&scopes, "scope", "read", "comma-separated scopes: read, act, land, admin (read is always added)")
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
			fmt.Fprintln(tw, "NAME\tSCOPES\tEXPIRES\tSTATE")
			now := time.Now()
			for _, t := range toks {
				state := "live"
				switch {
				case t.Revoked != nil:
					state = "revoked"
				case !now.Before(t.Expires):
					state = "expired"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", t.Name, joinScopes(t.Scopes), t.Expires.Local().Format(time.DateTime), state)
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
