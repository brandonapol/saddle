package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/brief"
)

func briefCmd() *cobra.Command {
	var (
		width  int
		watch  bool
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "brief [task]",
		Short: "Show an agent's brief: goal, owned and hands-off paths, done-when, children",
		Long: `Show a compact brief for one task: its goal, the paths it owns, the paths
other tasks own (hands off), the done-when checklist from its prompt, and the
tasks it spawned. With no task, uses SADDLE_TASK or the current worktree's
task. --watch redraws it every two seconds.`,
		Args: cobra.MaximumNArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			flag := ""
			if len(args) == 1 {
				flag = args[0]
			}
			id, err := resolveTask(a, flag)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				b, err := brief.Load(a, id)
				if err != nil {
					return err
				}
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(b)
			}
			if !watch {
				return writeTaskBrief(out, a, id, width)
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			tick := time.NewTicker(2 * time.Second)
			defer tick.Stop()
			for {
				fmt.Fprint(out, "\x1b[H\x1b[2J")
				if err := writeTaskBrief(out, a, id, width); err != nil {
					return err
				}
				select {
				case <-ctx.Done():
					return nil
				case <-tick.C:
				}
			}
		}),
	}
	cmd.Flags().IntVar(&width, "width", 80, "wrap the brief to this many columns")
	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "redraw every two seconds until interrupted")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// writeTaskBrief prints task id's brief wrapped to width.
func writeTaskBrief(w io.Writer, a *app.App, id string, width int) error {
	b, err := brief.Load(a, id)
	if err != nil {
		return err
	}
	for _, l := range b.Lines(width) {
		fmt.Fprintln(w, l)
	}
	return nil
}
