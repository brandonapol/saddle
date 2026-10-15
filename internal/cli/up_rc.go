package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/orchrc"
	"github.com/brandonapol/saddle/internal/remote"
	"github.com/brandonapol/saddle/internal/tui"
)

// addRCFlag adds saddle up's --rc (#409) and returns whether a run wants
// the Remote Control orchestrator: the flag, or [orchestrator]
// remote_control in config.
func addRCFlag(cmd *cobra.Command) func(a *app.App) bool {
	var rc bool
	cmd.Flags().BoolVar(&rc, "rc", false, "run the orchestrator as an interactive Claude Code session with Remote Control on, to steer it from the Claude app ([orchestrator] remote_control)")
	return func(a *app.App) bool { return rc || a.Cfg.Orchestrator.RemoteControl }
}

// upLock is the engine-lock owner for a saddle up run.
func upLock(rc bool) string {
	if rc {
		return app.LockUpRC
	}
	return app.LockUp
}

// runUpTUI runs the TUI: around the headless orchestrator, or with rc
// around the Remote Control one, whose first message is first. While it
// runs, a sleep inhibitor is held whenever autopilot is on.
func runUpTUI(ctx context.Context, out io.Writer, a *app.App, first string, rc bool, opts ...tui.Option) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go orchrc.KeepAwake(ctx, a)
	if !rc {
		return tui.Run(a, first, opts...)
	}
	s, err := orchrc.Start(a, first, remotePushOn())
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Orchestrator: Claude Code session %q under Remote Control (tmux window %s).\n", s.Name, s.Window)
	return tui.RunRC(a, s, opts...)
}

// remotePushOn reports whether [remote.push] is configured, so saddle pushes
// interrupt notices itself.
func remotePushOn() bool {
	cfg, err := remote.LoadConfig(remote.UserConfigPath())
	return err == nil && cfg.Push.On()
}
