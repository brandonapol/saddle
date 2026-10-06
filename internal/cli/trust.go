package cli

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/brandonapol/saddle/internal/trust"
	"github.com/spf13/cobra"
)

// stdinIsTTY decides whether a person can answer the trust prompt; tests
// replace it.
var stdinIsTTY = func(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	return trust.IsTerminal(f)
}

// repoDir is where saddle works: SADDLE_ROOT, else the working directory.
func repoDir() string {
	if r := os.Getenv("SADDLE_ROOT"); r != "" {
		return r
	}
	return wd()
}

// gateTrust asks whether saddle may work in this repo, unless a decision is
// remembered (#215). yes is --trust. It must run before anything under
// .saddle/ is opened: app.Open creates the state database.
func gateTrust(out io.Writer, in io.Reader, yes bool) error {
	return trust.Gate(repoDir(), trust.Options{Yes: yes, Interactive: stdinIsTTY(in), In: in, Out: out})
}

// upTrustErr turns a refusal into what saddle up says.
func upTrustErr(err error) error {
	if errors.Is(err, trust.ErrDeclined) || errors.Is(err, trust.ErrUntrusted) {
		return fmt.Errorf("saddle up won't start in a folder you haven't trusted. Run `saddle trust` here to review what saddle does and trust it, or pass --trust: %w", err)
	}
	return err
}

func trustCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "trust",
		Short: "Trust this repo: let saddle set up and run agents here",
		Long: `Shows what saddle does in this repo and asks whether you trust it. The
decision is remembered in ~/.config/saddle/trust.json (or $XDG_CONFIG_HOME),
keyed by the repo's path and origin URL: moving the repo or changing its origin
asks again. saddle init and saddle up ask the same question themselves.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			in := cmd.InOrStdin()
			out := cmd.OutOrStdout()
			err := trust.Decide(repoDir(), trust.Options{Yes: yes, Interactive: stdinIsTTY(in), In: in, Out: out})
			if errors.Is(err, trust.ErrUntrusted) {
				return errors.New("not trusted: run `saddle trust` on a terminal to answer, or `saddle trust --yes` to trust it")
			}
			if err != nil {
				return err
			}
			rep, err := trust.Status(repoDir(), nil)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "trusted %s\n", rep.Repo.Path)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "trust without asking")
	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show whether saddle trusts this repo",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rep, err := trust.Status(repoDir(), nil)
			if err != nil {
				return err
			}
			writeTrustStatus(cmd.OutOrStdout(), rep)
			return nil
		},
	})
	return cmd
}

func writeTrustStatus(w io.Writer, rep trust.Report) {
	fmt.Fprintf(w, "%s: %s\n", rep.Repo.Path, rep.State)
	origin := rep.Repo.Origin
	if origin == "" {
		origin = "(none)"
	}
	fmt.Fprintf(w, "  origin: %s\n", origin)
	switch rep.State {
	case trust.StateTrusted:
		fmt.Fprintf(w, "  trusted at %s\n", rep.Recorded.TrustedAt.Format("2006-01-02 15:04 MST"))
	case trust.StateOriginChanged:
		fmt.Fprintf(w, "  trusted with origin %q; run `saddle trust` to trust the new one\n", rep.Recorded.Origin)
	default:
		fmt.Fprintln(w, "  run `saddle trust` to trust it")
	}
	if trust.Inherited() {
		fmt.Fprintln(w, "  this run is trusted anyway (SADDLE_TASK or SADDLE_TRUST=1 is set)")
	}
	fmt.Fprintf(w, "  decisions: %s\n", rep.Store)
}

func untrustCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "untrust",
		Short: "Forget that this repo is trusted (saddle up and init will ask again)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := trust.Default()
			if err != nil {
				return err
			}
			r, err := trust.Identify(repoDir())
			if err != nil {
				return err
			}
			if err := s.Forget(r); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "saddle no longer trusts %s; saddle up and init will ask again. .saddle/, its hooks and running agents are left as they are (saddle down stops agents).\n", r.Path)
			return nil
		},
	}
}
