package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

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
	// backend opens the spawn backend; nil with dryRun.
	backend func() (sreBackend, error)
}

// sreBackend spawns investigators and holds the guard's incident (#550)
// under guardRoot, saddle's main checkout.
type sreBackend struct {
	spawner sre.Spawner
	// nextID picks the id the next investigator spawns under, so intake can
	// guard it before it starts (#567).
	nextID    func() (string, error)
	guardRoot string
	close     func()
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
paths: the files the incident names. The investigators are held read-only
by the guard (saddle sre incident). --dry-run prints the plan without
spawning. See docs/sre.md.`
	cmd.Args = cobra.ArbitraryArgs
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		o.runner = sre.ExecRunner{}
		if !o.dryRun {
			o.backend = openSREBackend
		}
		return runSRE(cmd.Context(), cmd.OutOrStdout(), cmd.InOrStdin(), args, o)
	}
	f := cmd.Flags()
	f.StringVar(&o.root, "root", "", "repo root (default: the current repo)")
	f.BoolVar(&o.dryRun, "dry-run", false, "run the checks and print the plan; spawn nothing")
}

func openSREBackend() (sreBackend, error) {
	a, err := open()
	if err != nil {
		return sreBackend{}, err
	}
	if err := a.UseScratch(); err != nil {
		fmt.Fprintln(os.Stderr, "warning: scratch root: "+err.Error())
	}
	return sreBackend{
		spawner:   appSpawner{a: a, parent: os.Getenv("SADDLE_TASK")},
		nextID:    a.Store.NextID,
		guardRoot: a.Root,
		close:     func() { a.Close() },
	}, nil
}

// appSpawner starts investigators through saddle's normal spawn.
type appSpawner struct {
	a      *app.App
	parent string
}

func (s appSpawner) Spawn(sp sre.SpawnSpec) (string, error) {
	// Confirm: a suspect file has usually just landed, and investigating it
	// is new work, not a stack repair.
	t, err := s.a.Spawn(app.SpawnReq{ID: sp.ID, Title: sp.Title, Prompt: sp.Prompt, Claims: sp.Claims, Parent: s.parent, Confirm: true})
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
	if o.backend == nil {
		fmt.Fprintln(w, "dry run: no investigators spawned")
		return nil
	}
	b, err := o.backend()
	if err != nil {
		return err
	}
	defer b.close()
	// The guard holds the investigators read-only (#550). The incident opens
	// first, so an open one refuses this intake before anything spawns.
	now := time.Now()
	inc := &sre.Incident{ID: "inc-" + now.UTC().Format("20060102-150405"), Summary: p.Intake.Summary, Opened: now.UTC()}
	inc.AllowChecks(p.Config)
	if err := sre.OpenIncident(b.guardRoot, inc); err != nil {
		return err
	}
	got, err := sre.Launch(p, guardedSpawner{b.spawner, b.nextID, b.guardRoot})
	for _, l := range got {
		fmt.Fprintf(w, "spawned %s for lane %s\n", l.Task, l.Lane)
	}
	if len(got) == 0 {
		// No investigators: an open incident would only block the next intake.
		_ = sre.UpdateIncident(b.guardRoot, func(i *sre.Incident) error { i.Close(time.Now()); return nil })
	} else {
		fmt.Fprintf(w, "incident %s open: production is read-only for the investigators (saddle sre incident)\n", inc.ID)
	}
	return err
}

// guardedSpawner adds each investigator to the open incident before it
// starts: it reserves the task id, guards it, then spawns under that id, so
// the investigator's first tool call is already read-only (#567). A spawn
// that fails gives the reservation back.
type guardedSpawner struct {
	sre.Spawner
	nextID func() (string, error)
	root   string
}

func (g guardedSpawner) Spawn(s sre.SpawnSpec) (string, error) {
	id, err := g.nextID()
	if err != nil {
		return "", err
	}
	if err := g.guard(id); err != nil {
		return "", fmt.Errorf("can't guard %s read-only, so it was not started: %w", id, err)
	}
	s.ID = id
	got, err := g.Spawner.Spawn(s)
	if err != nil {
		g.unguard(id)
		return "", err
	}
	if got != id {
		// The spawner ignored the reserved id: guard what it started and
		// free the reservation, which a later, unrelated task may take.
		if err := g.guard(got); err != nil {
			return "", fmt.Errorf("%s started but is not guarded read-only (kill it): %w", got, err)
		}
		g.unguard(id)
	}
	return got, nil
}

func (g guardedSpawner) unguard(id string) {
	_ = sre.UpdateIncident(g.root, func(i *sre.Incident) error {
		i.Investigators = slices.DeleteFunc(i.Investigators, func(t string) bool { return t == id })
		return nil
	})
}

func (g guardedSpawner) guard(id string) error {
	return sre.UpdateIncident(g.root, func(i *sre.Incident) error {
		if !slices.Contains(i.Investigators, id) {
			i.Investigators = append(i.Investigators, id)
		}
		return nil
	})
}
