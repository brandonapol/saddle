package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/sre"
	"github.com/spf13/cobra"
)

// sreOpts are saddle sre's inputs, split out so tests inject fakes.
type sreOpts struct {
	root   string
	dryRun bool
	runner sre.Runner
	// spawner opens the spawn backend; nil with dryRun.
	spawner func() (sre.Spawner, func(), error)
}

// addSREIntake makes the sre command itself the incident intake.
func addSREIntake(cmd *cobra.Command) {
	var o sreOpts
	cmd.Use = "sre [incident text | -]"
	cmd.Short = "SRE mode: take an incident, run preemptive checks, spawn read-only investigators"
	cmd.Long = `Takes pasted incident text (arguments, or stdin when none or "-"), runs the
preemptive checks from .saddle/sre.toml read-only, then plans 3-4 disjoint
lanes (code diffs and deploys, logs and errors, infrastructure, dependencies)
and spawns one read-only investigator per lane. Only the code lane claims
paths: the files the incident names. --dry-run prints the plan without
spawning. See docs/sre.md.`
	cmd.Args = cobra.ArbitraryArgs
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		o.runner = sre.ExecRunner{}
		if !o.dryRun {
			o.spawner = openSpawner
		}
		return runSRE(cmd.Context(), cmd.OutOrStdout(), cmd.InOrStdin(), args, o)
	}
	f := cmd.Flags()
	f.StringVar(&o.root, "root", "", "repo root (default: the current repo)")
	f.BoolVar(&o.dryRun, "dry-run", false, "run the checks and print the plan; spawn nothing")
}

func openSpawner() (sre.Spawner, func(), error) {
	a, err := open()
	if err != nil {
		return nil, nil, err
	}
	if err := a.UseScratch(); err != nil {
		fmt.Fprintln(os.Stderr, "warning: scratch root: "+err.Error())
	}
	return appSpawner{a: a, parent: os.Getenv("SADDLE_TASK")}, func() { a.Close() }, nil
}

// appSpawner starts investigators through saddle's normal spawn.
type appSpawner struct {
	a      *app.App
	parent string
}

func (s appSpawner) Spawn(sp sre.SpawnSpec) (string, error) {
	// Confirm: a suspect file has usually just landed, and investigating it
	// is new work, not a stack repair.
	t, err := s.a.Spawn(app.SpawnReq{Title: sp.Title, Prompt: sp.Prompt, Claims: sp.Claims, Parent: s.parent, Confirm: true})
	return t.ID, err
}

func runSRE(ctx context.Context, w io.Writer, in io.Reader, args []string, o sreOpts) error {
	if ctx == nil {
		ctx = context.Background()
	}
	var text string
	if len(args) == 0 || len(args) == 1 && args[0] == "-" {
		b, err := io.ReadAll(in)
		if err != nil {
			return err
		}
		text = string(b)
	} else {
		text = strings.Join(args, " ")
	}
	root := o.root
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		if root, err = gitx.Root(wd); err != nil {
			return err
		}
	}
	cfg, err := sre.Load(root)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(w, "no %s: no preemptive checks; investigators will propose some\n", sre.File)
		cfg, err = nil, nil
	}
	if err != nil {
		return err
	}
	p, err := sre.Build(ctx, text, cfg, root, os.DirFS(root), o.runner)
	if err != nil {
		return err
	}
	for _, r := range p.Checks {
		line := fmt.Sprintf("check %s: %s", r.Name, r.Status)
		if r.Note != "" {
			line += " (" + r.Note + ")"
		}
		fmt.Fprintln(w, line)
	}
	for _, l := range p.Lanes {
		line := "lane " + string(l.Kind)
		if len(l.Claims) > 0 {
			line += ": claims " + strings.Join(l.Claims, ", ")
		}
		if len(l.Checks) > 0 {
			line += "; checks " + strings.Join(l.Checks, ", ")
		}
		fmt.Fprintln(w, line)
	}
	if o.spawner == nil {
		fmt.Fprintln(w, "dry run: no investigators spawned")
		return nil
	}
	sp, closeFn, err := o.spawner()
	if err != nil {
		return err
	}
	defer closeFn()
	got, err := sre.Launch(p, sp)
	for _, l := range got {
		fmt.Fprintf(w, "spawned %s for lane %s\n", l.Task, l.Lane)
	}
	return err
}
