package sre

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

// Plan is one incident's intake: the preemptive check results and the lanes
// to investigate.
type Plan struct {
	Incident Incident
	Config   *Config
	Checks   []CheckResult
	Lanes    []Lane
}

// Build runs cfg's preemptive checks read-only in dir, then plans the lanes.
// cfg may be nil when the repo has no .saddle/sre.toml yet.
func Build(ctx context.Context, text string, cfg *Config, dir string, repo fs.FS, r Runner) (*Plan, error) {
	inc := ParseIncident(text)
	if inc.Text == "" {
		return nil, errors.New("sre: incident text is empty; paste it as an argument or on stdin")
	}
	if cfg == nil {
		cfg = &Config{}
	}
	p := &Plan{Incident: inc, Config: cfg, Checks: RunChecks(ctx, cfg, dir, r)}
	p.Lanes = PlanLanes(inc, cfg, p.Checks, repo)
	return p, nil
}

// SpawnSpec is one investigator to start.
type SpawnSpec struct {
	Lane   LaneKind
	Title  string
	Prompt string
	Claims []string
}

// Spawner starts an investigator and returns its task id. The CLI backs it
// with saddle's spawn; tests use a fake.
type Spawner interface {
	Spawn(SpawnSpec) (string, error)
}

// Launched is a started investigator.
type Launched struct {
	Lane LaneKind
	Task string
}

// Spec is the spawn request for one lane of p.
func (p *Plan) Spec(l Lane) SpawnSpec {
	return SpawnSpec{
		Lane:   l.Kind,
		Title:  fmt.Sprintf("sre %s: %s", l.Kind, p.Incident.Summary),
		Prompt: p.Prompt(l),
		Claims: l.Claims,
	}
}

// Launch spawns one investigator per lane. It refuses overlapping claims
// before spawning anything; a failed lane does not stop the others.
func Launch(p *Plan, s Spawner) ([]Launched, error) {
	if err := CheckDisjoint(p.Lanes); err != nil {
		return nil, err
	}
	var out []Launched
	var errs []error
	for _, l := range p.Lanes {
		id, err := s.Spawn(p.Spec(l))
		if err != nil {
			errs = append(errs, fmt.Errorf("lane %s: %w", l.Kind, err))
			continue
		}
		out = append(out, Launched{Lane: l.Kind, Task: id})
	}
	return out, errors.Join(errs...)
}

// Prompt is the investigator brief for lane l.
func (p *Plan) Prompt(l Lane) string {
	var b strings.Builder
	fmt.Fprintf(&b, "SRE incident investigation, lane: %s.\n\n%s\n\n", l.Kind, l.Focus)
	b.WriteString(`Rules:
- You are a read-only investigator. Do not edit files, commit, deploy, restart,
  scale or run anything that changes production or shared state.
- Credentials are references (env var names, contexts); never print their values.
- Report evidence with timestamps and sources, then your best root-cause
  hypothesis and how confident you are. Another agent must verify a root cause
  before any fix is proposed, and a mutating fix goes through the decision queue.
- Note checks worth adding to .saddle/sre.toml so the runbook grows.
- Finish with the saddle done tool; your summary is your report.
`)
	if len(l.Claims) > 0 {
		fmt.Fprintf(&b, "\nFiles the incident names (claimed for this lane): %s\n", strings.Join(l.Claims, ", "))
	}
	if len(l.Endpoints) > 0 {
		b.WriteString("\nEndpoints:\n")
		for _, n := range l.Endpoints {
			e := p.Config.Endpoints[n]
			fmt.Fprintf(&b, "- %s: %s%s\n", n, e.URL, p.credRef(e.Credential))
		}
	}
	if len(p.Checks) > 0 {
		mine := map[string]bool{}
		for _, n := range l.Checks {
			mine[n] = true
		}
		b.WriteString("\nPreemptive checks (already run read-only):\n")
		for _, r := range p.Checks {
			tag := ""
			if mine[r.Name] {
				tag = " [this lane follows up]"
			}
			fmt.Fprintf(&b, "- %s: %s, expect %q%s%s\n", r.Name, r.Status, r.Expect, p.checkCreds(r.Name), tag)
			if r.Note != "" {
				fmt.Fprintf(&b, "  note: %s\n", r.Note)
			}
			if r.Output != "" {
				fmt.Fprintf(&b, "  output:\n    %s\n", strings.ReplaceAll(r.Output, "\n", "\n    "))
			}
		}
	} else {
		b.WriteString("\nNo preemptive checks are configured (.saddle/sre.toml); propose some.\n")
	}
	fmt.Fprintf(&b, "\nIncident as reported:\n\n%s\n", p.Incident.Text)
	return b.String()
}

func (p *Plan) checkCreds(name string) string {
	for _, ch := range p.Config.Checks {
		if ch.Name != name || len(ch.Credentials) == 0 {
			continue
		}
		var refs []string
		for _, c := range ch.Credentials {
			refs = append(refs, strings.TrimPrefix(p.credRef(c), " via "))
		}
		return " via " + strings.Join(refs, ", ")
	}
	return ""
}

func (p *Plan) credRef(name string) string {
	c, ok := p.Config.Credentials[name]
	switch {
	case !ok:
		return ""
	case c.Env != "":
		return fmt.Sprintf(" via %s (env %s)", name, c.Env)
	default:
		return fmt.Sprintf(" via %s (%s)", name, c.Ref)
	}
}
