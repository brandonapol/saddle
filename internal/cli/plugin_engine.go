package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/engine"
	"github.com/brandonapol/saddle/internal/supervise"
	"github.com/brandonapol/saddle/internal/trust"
	"github.com/spf13/cobra"
)

func pluginEngineCmd() *cobra.Command {
	var supervised, stop bool
	cmd := &cobra.Command{
		Use:   "engine",
		Short: "Watch agents for the orchestrating Claude Code session (runs until stopped)",
		Long: `Runs what saddle up runs in the background, without the TUI: it notices
agents waiting on a prompt or stopped without calling done and queues those
events for the orchestrator, and runs the stack sentinel, CI watcher and
auto-merge watcher. While it runs, the plugin's hooks deliver the events into
this repo's orchestrating Claude Code session. It can't run alongside saddle up.

The engine stays up unattended (#789). With --supervise it runs as a child
that is started again whenever it exits. Either way, if it dies without being
stopped on purpose, the next saddle tool call or hook from any agent starts
it again in the background. Ctrl-c, saddle down, saddle up taking the repo
over, or saddle plugin engine --stop stop it for good.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, why := openPlugin(wd())
			if a == nil {
				return errors.New(why)
			}
			defer a.Close()
			switch {
			case stop:
				if a.StopEngine() {
					fmt.Fprintln(cmd.OutOrStdout(), "saddle engine stopping; nothing restarts it.")
				} else {
					fmt.Fprintln(cmd.OutOrStdout(), "saddle engine is not running; nothing will restart it.")
				}
				return nil
			case supervised:
				return superviseEngine(cmd.OutOrStdout(), cmd.ErrOrStderr(), a)
			}
			return runEngine(cmd.OutOrStdout(), a)
		},
	}
	cmd.Flags().BoolVar(&supervised, "supervise", false, "run the engine as a child and start it again whenever it exits, until stopped on purpose")
	cmd.Flags().BoolVar(&stop, "stop", false, "stop this repo's engine, and its supervisor, for good")
	return cmd
}

func engineGate(a *app.App) error {
	if err := trust.Gate(a.Root, trust.Options{}); err != nil {
		return fmt.Errorf("the saddle engine won't run in a repo you haven't trusted; run `saddle trust` in %s first: %w", a.Root, err)
	}
	return nil
}

// engineSignalContext ends on SIGINT or SIGTERM; interrupted reports whether
// it was SIGINT (ctrl-c, saddle down): a stop on purpose, which nothing undoes.
func engineSignalContext() (ctx context.Context, cancel context.CancelFunc, interrupted func() bool) {
	ctx, cancel = context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	var intr atomic.Bool
	go func() {
		select {
		case s := <-sigs:
			intr.Store(s == os.Interrupt)
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() { signal.Stop(sigs); cancel() }, intr.Load
}

func runEngine(w io.Writer, a *app.App) error {
	if err := engineGate(a); err != nil {
		return err
	}
	if _, err := a.EnsureOrchestrator(); err != nil {
		return err
	}
	release, err := a.AcquireLock(app.LockEngine)
	if err != nil {
		return err
	}
	defer release()
	if os.Getenv(app.EngineSupervisedEnv) == "" {
		_ = a.WantEngine(os.Getpid()) // its supervisor keeps the marker otherwise
	}
	ctx, stop, interrupted := engineSignalContext()
	defer stop()
	fmt.Fprintf(w, "saddle engine running for %s. Stop it with ctrl-c; agents keep running.\n", a.Root)
	stopWatchers := startWatchers(ctx, a)
	err = engine.New(a).Run(ctx)
	stopWatchers()
	if interrupted() {
		a.UnwantEngine()
	}
	return err
}

// superviseEngine runs `saddle plugin engine` as a child and starts it again
// whenever it exits, until stopped on purpose (#789).
func superviseEngine(w, ew io.Writer, a *app.App) error {
	if err := engineGate(a); err != nil {
		return err
	}
	if owner := a.LockOwner(); owner != "" {
		if owner == app.LockEngine {
			fmt.Fprintln(w, "saddle engine is already running for "+a.Root+".")
			return nil
		}
		return errors.New("saddle up is running for this repo; the engine can't run alongside it")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := a.WantEngine(os.Getpid()); err != nil {
		return err
	}
	ctx, stop, interrupted := engineSignalContext()
	defer stop()
	ours := func() bool { pid, ok := a.EngineWanted(); return ok && pid == os.Getpid() }
	fmt.Fprintf(w, "saddle engine supervisor (pid %d) for %s: the engine restarts whenever it exits.\n", os.Getpid(), a.Root)
	supervise.Respawn(ctx, supervise.RespawnOptions{
		Keep: ours,
		OnExit: func(err error, wait time.Duration) {
			why := "exited"
			if err != nil {
				why = err.Error()
			}
			msg := fmt.Sprintf("the engine stopped (%s); its supervisor starts it again in %s", why, wait)
			fmt.Fprintln(ew, "saddle engine: "+msg)
			a.Store.Event("", app.EventEngineRestarted, msg)
		},
	}, func(ctx context.Context) error {
		c := exec.CommandContext(ctx, exe, "plugin", "engine")
		c.Dir, c.Stdout, c.Stderr = a.Root, w, ew
		c.Env = append(os.Environ(), app.EngineSupervisedEnv+"=1")
		c.Cancel = func() error {
			if interrupted() {
				return c.Process.Signal(os.Interrupt)
			}
			return c.Process.Signal(syscall.SIGTERM)
		}
		c.WaitDelay = 30 * time.Second
		return c.Run()
	})
	if interrupted() && ours() {
		a.UnwantEngine()
	}
	return nil
}
