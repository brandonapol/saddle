package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/banner"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/doctor"
	"github.com/spf13/cobra"
)

func initCmd() *cobra.Command {
	var quiet, trusted bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create .saddle/ with a config template",
		Long: `Creates .saddle/ with a config template and installs saddle's git hooks.
The first time in a repo it asks whether you trust it, before writing anything.
Without a terminal, pass --trust (or set SADDLE_TRUST=1).

Then it says which git provider CLI (gh, then glab) and which agent CLIs
(claude, codex, grok) it found. When claude can't run but another agent CLI
can, it makes that one the default harness in .saddle/config.toml. With no
agent CLI at all it prints how to install one.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			banner.Print(cmd.OutOrStdout(), banner.Options{Quiet: quiet, IsTTY: stdoutIsTTY})
			if err := gateTrust(cmd.OutOrStdout(), cmd.InOrStdin(), trusted); err != nil {
				return err
			}
			return withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
				if err := a.Init(); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "initialized %s/.saddle (edit .saddle/config.toml)\n", a.Root)
				return initSetup(cmd.OutOrStdout(), a.Root)
			})(cmd, args)
		},
	}
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "don't print the howdy banner")
	cmd.Flags().BoolVar(&trusted, "trust", false, "trust this repo without asking (remembered; see saddle trust)")
	return cmd
}

// initSetup reports the git provider CLI and the agent CLIs saddle init
// found in the repo at root (#646). When the harness can't run but another
// agent CLI can, it writes that one as harness, unless the owner set harness
// in .saddle/config.toml. No agent CLI is not an error: it prints how to
// install one.
func initSetup(out io.Writer, root string) error {
	cfg, err := config.Load(root)
	if err != nil {
		return err
	}
	env := doctor.System(root)
	f := doctor.DetectForge(env, originURL(env), cfg.Forge)
	fmt.Fprintln(out, "git provider: "+f.Detail)
	if f.Fix != "" {
		fmt.Fprintln(out, "  fix: "+f.Fix)
	}
	ss := agent.Availability(doctor.AgentCmds(cfg))
	order := doctor.HarnessOrder(cfg)
	fb, ok := agent.FallbackHarness(cfg.Harness, order, ss)
	if ok && !okIn(ss, fb) {
		// Installed, but no credentials file saddle knows: say so, and keep going.
		fmt.Fprintf(out, "agent: %s looks logged out (%s); run %s if it isn't logged in some other way\n",
			fb, reasonOf(ss, fb), agent.LoginHint(fb))
		if fb == cfg.Harness {
			return nil
		}
	}
	if !ok {
		fmt.Fprintln(out, "no agent CLI is installed and logged in. Install one, then run saddle init again:")
		fmt.Fprintln(out, agent.InstallSteps(order))
		return nil
	}
	usable := strings.Join(agent.Usable(ss), ", ")
	if fb == cfg.Harness {
		fmt.Fprintf(out, "agent: %s (usable: %s)\n", fb, usable)
	} else {
		fmt.Fprintf(out, "agent: %s can't run (%s); %s can (usable: %s)\n", cfg.Harness, reasonOf(ss, cfg.Harness), fb, usable)
		set, err := harnessSet(root)
		if err != nil {
			return err
		}
		if set {
			fmt.Fprintf(out, "  set harness = %q in .saddle/config.toml to use it by default; saddle up falls back to it meanwhile\n", fb)
		} else {
			if err := writeHarness(root, fb); err != nil {
				return err
			}
			fmt.Fprintf(out, "  wrote harness = %q to .saddle/config.toml\n", fb)
		}
		fmt.Fprintf(out, "  to run %s instead: %s\n", cfg.Harness, agent.InstallHint(cfg.Harness))
	}
	if fb == config.HarnessCodex {
		fmt.Fprintln(out, "  note: "+doctor.NoHooksNote)
	}
	cfg.Harness = fb
	return initOrchestrator(out, root, cfg, ss)
}

// initOrchestrator writes [orchestrator] adapter when the orchestrator
// can't run on Claude Code here and another headless agent can (#668),
// unless the owner set it. Under harness = "grok" the orchestrator already
// follows the harness, so nothing is written.
func initOrchestrator(out io.Writer, root string, cfg config.Config, ss []agent.Status) error {
	oa := agent.OrchestratorAdapter(cfg, ss)
	if cfg.Orchestrator.Adapter != "" {
		fmt.Fprintf(out, "orchestrator: %s ([orchestrator] adapter)\n", oa)
		return nil
	}
	if oa == config.HarnessClaude || cfg.Harness == config.HarnessGrok && oa == config.HarnessGrok {
		return nil
	}
	if err := writeOrchestratorAdapter(root, oa); err != nil {
		return err
	}
	fmt.Fprintf(out, "orchestrator: claude can't run; wrote [orchestrator] adapter = %q to .saddle/config.toml\n", oa)
	if oa == config.HarnessCodex {
		fmt.Fprintln(out, "  note: a codex orchestrator has no hooks, so saddle's events reach it between turns, not mid-turn")
	}
	return nil
}

var (
	orchestratorHeader  = regexp.MustCompile(`(?m)^\[orchestrator\][ \t]*$`)
	templateOrchAdapter = regexp.MustCompile(`(?m)^#\s*adapter\s*=.*$`)
)

// writeOrchestratorAdapter sets [orchestrator] adapter = name in the repo's
// config.toml: on the template's commented-out line when it is there, else
// right under an [orchestrator] header, else in a new section at the end.
func writeOrchestratorAdapter(root, name string) error {
	p := repoConfig(root)
	b, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	line := fmt.Sprintf("adapter = %q          # saddle init: claude can't run here", name)
	s := string(b)
	h := orchestratorHeader.FindStringIndex(s)
	switch {
	case h != nil:
		// Only the template line inside [orchestrator]: the next section ends it.
		rest := s[h[1]:]
		end := len(rest)
		if i := regexp.MustCompile(`(?m)^\[`).FindStringIndex(rest); i != nil {
			end = i[0]
		}
		if loc := templateOrchAdapter.FindStringIndex(rest[:end]); loc != nil {
			s = s[:h[1]] + rest[:loc[0]] + line + rest[loc[1]:]
		} else {
			s = s[:h[1]] + "\n" + line + rest
		}
	default:
		if s != "" && !strings.HasSuffix(s, "\n") {
			s += "\n"
		}
		s += "\n[orchestrator]\n" + line + "\n"
	}
	return os.WriteFile(p, []byte(s), 0o644)
}

// originURL is origin's URL, else the first remote's, else "".
func originURL(env doctor.Env) string {
	if u, err := env.Git("remote", "get-url", "origin"); err == nil {
		return u
	}
	out, _ := env.Git("remote")
	if rs := strings.Fields(out); len(rs) > 0 {
		u, _ := env.Git("remote", "get-url", rs[0])
		return u
	}
	return ""
}

// okIn reports whether name can run, verified logged in.
func okIn(ss []agent.Status, name string) bool {
	for _, s := range ss {
		if s.Name == name {
			return s.OK
		}
	}
	return false
}

func reasonOf(ss []agent.Status, name string) string {
	for _, s := range ss {
		if s.Name == name {
			return s.Reason
		}
	}
	return "unknown adapter"
}

func repoConfig(root string) string { return filepath.Join(root, ".saddle", "config.toml") }

// harnessSet reports whether the repo's config.toml sets harness.
func harnessSet(root string) (bool, error) {
	var v map[string]any
	md, err := toml.DecodeFile(repoConfig(root), &v)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return md.IsDefined("harness"), err
}

var templateHarness = regexp.MustCompile(`(?m)^#\s*harness\s*=.*$`)

// writeHarness sets harness = name in the repo's config.toml: on the
// template's commented-out line when it is there, else first in the file.
func writeHarness(root, name string) error {
	p := repoConfig(root)
	b, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	line := fmt.Sprintf("harness = %q          # saddle init: the first agent CLI that can run here", name)
	s := string(b)
	if loc := templateHarness.FindStringIndex(s); loc != nil {
		s = s[:loc[0]] + line + s[loc[1]:]
	} else {
		s = line + "\n" + s
	}
	return os.WriteFile(p, []byte(s), 0o644)
}
