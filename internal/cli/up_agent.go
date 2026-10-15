package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/doctor"
	"github.com/brandonapol/saddle/internal/gitx"
)

// upFallback picks the agent a bare saddle up runs when the configured
// harness can't run here (#646): the first in order that can, said in one
// line. It returns "" when the harness can run, and an error with install
// steps when no agent CLI can.
func upFallback(out io.Writer, cfg config.Config, ss []agent.Status) (string, error) {
	order := doctor.HarnessOrder(cfg)
	fb, ok := agent.FallbackHarness(cfg.Harness, order, ss)
	if !ok {
		return "", fmt.Errorf("no agent CLI is installed and logged in, so saddle up has nothing to run. Install one:\n%s", agent.InstallSteps(order))
	}
	if fb == cfg.Harness {
		return "", nil
	}
	fmt.Fprintf(out, "%s can't run (%s); running %s for this run. Set harness = %q in .saddle/config.toml to make it the default, or %s\n",
		cfg.Harness, reasonOf(ss, cfg.Harness), fb, fb, agent.InstallHint(cfg.Harness))
	if fb == config.HarnessCodex {
		fmt.Fprintln(out, "note: "+doctor.NoHooksNote)
	}
	return fb, nil
}

// applyUpFallback runs upFallback for the repo saddle up runs in and makes
// its pick this run's harness. It leaves anything it can't read to the
// checks after it.
func applyUpFallback(out io.Writer) error {
	root := os.Getenv("SADDLE_ROOT")
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil
		}
		if root, err = gitx.Root(wd); err != nil {
			return nil
		}
	}
	cfg, err := config.Load(root)
	if err != nil {
		return nil
	}
	h, err := upFallback(out, cfg, agent.Availability(doctor.AgentCmds(cfg)))
	if err != nil {
		return err
	}
	return applyUpHarness(h)
}
