package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/runq"
	"github.com/spf13/cobra"
)

// exitFn ends saddle run with the command's exit code; tests replace it.
var exitFn = os.Exit

// exitTempFail is EX_TEMPFAIL: saddle run gave up waiting (--wait-max).
const exitTempFail = 75

// heavy is the queue as seen from the current directory. Inside a saddle
// repo it reads the repo's runq.toml and logs events to its state; anywhere
// else (another repo, a HOME-less shell) only the user config applies. The
// returned func closes what it opened.
func heavy(errOut io.Writer) (app.Heavy, func()) {
	root := os.Getenv("SADDLE_ROOT")
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = saddleRoot(wd)
		}
	}
	if root != "" {
		if a, err := app.Open(root); err == nil {
			h := a.Heavy()
			h.Warn = errOut
			return h, func() { _ = a.Close() }
		}
	}
	return app.Heavy{Root: root, Warn: errOut}, func() {}
}

// parsePrio reads train (or gate), worker, background, or a number.
func parsePrio(s string) (int, error) {
	switch s {
	case "train", "gate":
		return runq.PrioGate, nil
	case "worker":
		return runq.PrioWorker, nil
	case "background":
		return runq.PrioBackground, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("--prio %q: want train, worker, background or a number", s)
	}
	return n, nil
}

func heavyRunCmd() *cobra.Command {
	var class, prio string
	var waitMax time.Duration
	cmd := &cobra.Command{
		Use:   "run --class <class> [--prio train|worker|background] [--wait-max 30m] -- <cmd> [args...]",
		Short: "Run a CPU-heavy command when the machine's heavy-run queue gives it a slot",
		Long: `Runs a CPU-heavy command (tests, linters, e2e suites) through the heavy-run
queue every saddle session, agent and repo on this machine shares. While it
waits it prints one line to stderr, again only when its place changes:

  queued: position 1 of 2 for go-test, holder t83 (make check, 40s), ~2m (starts automatically; ...)

It never polls hot, forwards stdin, stdout, signals and the exit code, and
passes SADDLE_RUNQ_LEASE to the command so heavy runs it starts ride on its
slot instead of queueing behind it.

Modes (SADDLE_RUNQ, else mode in .saddle/runq.toml or ~/.config/saddle/runq.toml):
  observe   record the run, never wait (the default while slots are sized)
  enforce   wait for a slot
  off       bypass the queue entirely

After --wait-max (default 30m, wait_max in runq.toml) it gives up with exit
code 75 naming the holder. Report that rather than retrying in a loop.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if class == "" {
				return errors.New("--class is required (e.g. go-test, flutter-test, golangci-lint, e2e, generic-heavy)")
			}
			p, err := parsePrio(prio)
			if err != nil {
				return err
			}
			if code := runHeavy(c, class, p, waitMax, args); code != 0 {
				exitFn(code)
			}
			return nil
		},
	}
	cmd.Flags().SetInterspersed(false) // everything after the command is its own
	cmd.Flags().StringVar(&class, "class", "", "run class (go-test, flutter-test, golangci-lint, e2e, generic-heavy, ...)")
	cmd.Flags().StringVar(&prio, "prio", "worker", "priority: train, worker, background, or a number")
	cmd.Flags().DurationVar(&waitMax, "wait-max", 0, "give up queueing after this long, exit 75 (default wait_max from runq.toml, else 30m)")
	return cmd
}

// runHeavy runs args through the queue and returns the exit code.
func runHeavy(c *cobra.Command, class string, prio int, waitMax time.Duration, args []string) int {
	errOut := c.ErrOrStderr()
	h, closeHeavy := heavy(errOut)
	defer closeHeavy()
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(sigs)
	child := exec.Command(args[0], args[1:]...)
	child.Stdin, child.Stdout, child.Stderr = c.InOrStdin(), c.OutOrStdout(), errOut
	_, err := h.Run(c.Context(), app.HeavyOpts{Class: class, Prio: prio, WaitMax: waitMax, Status: errOut, Signals: sigs}, child)
	return exitCode(errOut, err)
}

// exitCode maps a heavy run's error to saddle run's exit status, the way a
// shell would: the command's own code, 128+N for a signal, 127 when the
// command doesn't exist.
func exitCode(errOut io.Writer, err error) int {
	var exit *exec.ExitError
	var wm *runq.WaitMaxError
	var intr *runq.InterruptedError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return exit.ExitCode()
	case errors.As(err, &wm):
		fmt.Fprintln(errOut, "saddle run:", err)
		return exitTempFail
	case errors.As(err, &intr):
		fmt.Fprintln(errOut, "saddle run:", err)
		if s, ok := intr.Signal.(syscall.Signal); ok {
			return 128 + int(s)
		}
		return 130
	case errors.Is(err, exec.ErrNotFound):
		fmt.Fprintln(errOut, "saddle run:", err)
		return 127
	}
	fmt.Fprintln(errOut, "saddle run:", err)
	return 1
}

// runqStatusJSON is saddle runq status --json.
type runqStatusJSON struct {
	Mode    string          `json:"mode"`
	Path    string          `json:"path"`
	Classes []runqClassJSON `json:"classes"`
}

type runqClassJSON struct {
	Class   string          `json:"class"`
	Slots   int             `json:"slots"`
	Drained bool            `json:"drained,omitempty"`
	Holders []runqEntryJSON `json:"holders"`
	Waiters []runqEntryJSON `json:"waiters"`
}

type runqEntryJSON struct {
	Lease    string `json:"lease"`
	Label    string `json:"label"`
	Cmd      string `json:"cmd"`
	PID      int    `json:"pid"`
	Prio     int    `json:"prio"`
	Position int    `json:"position,omitempty"`
	AgeMS    int64  `json:"age_ms"`
}

func entriesJSON(es []runq.Entry) []runqEntryJSON {
	out := []runqEntryJSON{}
	for _, e := range es {
		out = append(out, runqEntryJSON{Lease: e.Token, Label: e.Label, Cmd: e.Cmd, PID: e.PID, Prio: e.Prio,
			Position: e.Position, AgeMS: e.Age.Milliseconds()})
	}
	return out
}

func shortLease(token string) string {
	if len(token) > 8 {
		return token[:8]
	}
	return token
}

// withQueue opens the queue for a runq subcommand.
func withQueue(fn func(c *cobra.Command, q *runq.Queue, args []string) error) func(*cobra.Command, []string) error {
	return func(c *cobra.Command, args []string) error {
		h, closeHeavy := heavy(c.ErrOrStderr())
		defer closeHeavy()
		q, _, err := h.Open()
		if err != nil {
			return err
		}
		defer func() { _ = q.Close() }()
		return fn(c, q, args)
	}
}

func runqCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runq",
		Short: "Inspect and steer the machine's heavy-run queue (saddle run)",
	}
	var asJSON bool
	status := &cobra.Command{
		Use:   "status",
		Short: "Show each class's slots, holders and waiters",
		Args:  cobra.NoArgs,
		RunE: withQueue(func(c *cobra.Command, q *runq.Queue, _ []string) error {
			st, err := q.Status()
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			if asJSON {
				js := runqStatusJSON{Mode: string(q.Mode()), Path: q.Path(), Classes: []runqClassJSON{}}
				for _, cs := range st {
					js.Classes = append(js.Classes, runqClassJSON{Class: cs.Class, Slots: cs.Slots, Drained: cs.Slots == 0,
						Holders: entriesJSON(cs.Holders), Waiters: entriesJSON(cs.Waiters)})
				}
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(js)
			}
			fmt.Fprintf(out, "mode: %s\n", q.Mode())
			for _, cs := range st {
				drained := ""
				if cs.Slots == 0 {
					drained = " (drained)"
				}
				fmt.Fprintf(out, "%s: %d/%d slots busy, %d waiting%s\n", cs.Class, len(cs.Holders), cs.Slots, len(cs.Waiters), drained)
				for _, h := range cs.Holders {
					fmt.Fprintf(out, "  running %s  lease %s  pid %d  %s (%s)\n", h.Label, shortLease(h.Token), h.PID, h.Cmd, h.Age.Round(time.Second))
				}
				for _, w := range cs.Waiters {
					fmt.Fprintf(out, "  #%d %s  lease %s  pid %d  prio %d  %s (waiting %s)\n", w.Position, w.Label, shortLease(w.Token), w.PID, w.Prio, w.Cmd, w.Age.Round(time.Second))
				}
			}
			return nil
		}),
	}
	status.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.AddCommand(
		status,
		&cobra.Command{
			Use:   "drain [class]",
			Short: "Set a class's slots (every class's without one) to 0: running work finishes, nothing new starts",
			Args:  cobra.MaximumNArgs(1),
			RunE: withQueue(func(c *cobra.Command, q *runq.Queue, args []string) error {
				class := ""
				if len(args) == 1 {
					class = args[0]
				}
				if err := q.Drain(class); err != nil {
					return err
				}
				fmt.Fprintln(c.OutOrStdout(), "drained; saddle runq slots <class> <n> resumes a class")
				return nil
			}),
		},
		&cobra.Command{
			Use:   "kill <lease>",
			Short: "Remove a holder or waiter by lease token (or a prefix of it)",
			Args:  cobra.ExactArgs(1),
			RunE: withQueue(func(c *cobra.Command, q *runq.Queue, args []string) error {
				if err := q.Kill(args[0]); err != nil {
					return err
				}
				fmt.Fprintf(c.OutOrStdout(), "killed lease %s\n", args[0])
				return nil
			}),
		},
		&cobra.Command{
			Use:   "slots <class> <n>",
			Short: "Set a class's slot count for every session on this machine",
			Args:  cobra.ExactArgs(2),
			RunE: withQueue(func(c *cobra.Command, q *runq.Queue, args []string) error {
				n, err := strconv.Atoi(args[1])
				if err != nil || n < 0 {
					return fmt.Errorf("slots %q: want a number >= 0", args[1])
				}
				if err := q.SetSlots(args[0], n); err != nil {
					return err
				}
				fmt.Fprintf(c.OutOrStdout(), "%s: %d slots\n", args[0], n)
				return nil
			}),
		},
	)
	return cmd
}
