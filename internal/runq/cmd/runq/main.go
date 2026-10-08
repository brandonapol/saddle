// Command runq is the spike's stand-in for `saddle run` and `saddle runq
// status` (#236). The e2e journeys drive it as two real contenders.
//
//	runq [-db path] run -class go-test [-prio 10] -- cmd args...
//	runq [-db path] status
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/brandonapol/saddle/internal/runq"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	top := flag.NewFlagSet("runq", flag.ContinueOnError)
	db := top.String("db", runq.DefaultPath(), "queue database")
	hb := top.Duration("heartbeat", 2*time.Second, "heartbeat interval")
	if err := top.Parse(args); err != nil {
		return 2
	}
	if top.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: runq [-db path] run|status")
		return 2
	}
	// The spike's journeys expect the prototype's behavior: enforce, one slot per class.
	q, err := runq.Open(runq.Options{Path: *db, Heartbeat: *hb, Mode: runq.ModeEnforce, Slots: map[string]int{}})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = q.Close() }()
	switch top.Arg(0) {
	case "status":
		st, err := q.Status()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		for _, c := range st {
			fmt.Printf("%s: %d/%d slots busy, %d waiting\n", c.Class, len(c.Holders), c.Slots, len(c.Waiters))
			for _, h := range c.Holders {
				fmt.Printf("  running %s pid %d %s (%s)\n", h.Label, h.PID, h.Cmd, h.Age.Round(time.Second))
			}
			for _, w := range c.Waiters {
				fmt.Printf("  #%d %s pid %d prio %d %s (waiting %s)\n", w.Position, w.Label, w.PID, w.Prio, w.Cmd, w.Age.Round(time.Second))
			}
		}
		return 0
	case "run":
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		class := fs.String("class", "", "run class")
		prio := fs.Int("prio", runq.PrioWorker, "priority")
		if err := fs.Parse(top.Args()[1:]); err != nil || *class == "" || fs.NArg() == 0 {
			fmt.Fprintln(os.Stderr, "usage: runq run -class C [-prio N] -- cmd args...")
			return 2
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		cmd := exec.CommandContext(ctx, fs.Arg(0), fs.Args()[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		err := q.Run(ctx, *class, *prio, cmd, os.Stderr)
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(os.Stderr, "runq: unknown command %q\n", top.Arg(0))
	return 2
}
