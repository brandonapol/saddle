// Package bench is saddle's collision bench (#599): a fixture repo, dummy
// tickets built to make parallel agents collide, a runner that works them
// with autopilot in a hermetic world, and a scorer that turns a run's event
// log into numbers. See docs/bench.md.
package bench

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/brandonapol/saddle/internal/autopilot"
	"github.com/brandonapol/saddle/internal/forecast"
)

// Scenario is one bench workload: tickets and what they should do.
type Scenario struct {
	Name string `toml:"-"`
	Dir  string `toml:"-"`
	// Description says what the scenario forces.
	Description string `toml:"description"`
	// Overlap is the forecast level the worst pair of tickets should get.
	Overlap string `toml:"overlap"`
	// SafeParallelism is how many tickets can run at once with no collision.
	SafeParallelism int `toml:"safe_parallelism"`
	// Concurrency is autopilot's cap; zero means one per ticket.
	Concurrency int      `toml:"concurrency"`
	Tickets     []Ticket `toml:"ticket"`
}

// Ticket is one dummy ticket, filed as issue Number.
type Ticket struct {
	Number int    `toml:"-"`
	File   string `toml:"file"`
	// WaitFor holds this ticket's fake agent before done until these
	// tickets' agents have finished their scripts.
	WaitFor []int  `toml:"wait_for"`
	Edits   []Edit `toml:"edit"`

	Title  string   `toml:"-"`
	Body   string   `toml:"-"`
	Claims []string `toml:"-"` // from the body's claims: line; nil for none
}

// Key names the ticket in forecasts and reports: "#1".
func (t Ticket) Key() string { return fmt.Sprintf("#%d", t.Number) }

// Edit is one change the fake agent makes. See docs/bench.md for the kinds.
type Edit struct {
	Path   string `toml:"path"`
	Old    string `toml:"old"`
	New    string `toml:"new"`
	Append bool   `toml:"append"`
	From   string `toml:"from"`
	Remove bool   `toml:"remove"`
}

// ScenariosDir is where scenarios live under the bench dir.
func ScenariosDir(benchDir string) string { return filepath.Join(benchDir, "scenarios") }

// FixtureDir is the fixture module under the bench dir.
func FixtureDir(benchDir string) string { return filepath.Join(benchDir, "fixture") }

// List names every scenario under benchDir, sorted.
func List(benchDir string) ([]string, error) {
	ents, err := os.ReadDir(ScenariosDir(benchDir))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if _, err := os.Stat(filepath.Join(ScenariosDir(benchDir), e.Name(), "scenario.toml")); e.IsDir() && err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// Load reads scenario name under benchDir.
func Load(benchDir, name string) (*Scenario, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return nil, fmt.Errorf("bad scenario name %q", name)
	}
	dir := filepath.Join(ScenariosDir(benchDir), name)
	sc := &Scenario{Name: name, Dir: dir}
	md, err := toml.DecodeFile(filepath.Join(dir, "scenario.toml"), sc)
	if err != nil {
		return nil, fmt.Errorf("scenario %s: %w", name, err)
	}
	if un := md.Undecoded(); len(un) > 0 {
		return nil, fmt.Errorf("scenario %s: unknown keys %v", name, un)
	}
	for i := range sc.Tickets {
		t := &sc.Tickets[i]
		t.Number = i + 1
		b, err := os.ReadFile(filepath.Join(dir, "tickets", t.File))
		if err != nil {
			return nil, fmt.Errorf("scenario %s ticket %s: %w", name, t.Key(), err)
		}
		t.Title, t.Body = splitTitle(string(b))
		t.Claims, _ = autopilot.TicketClaims(t.Body, nil)
	}
	if err := sc.validate(); err != nil {
		return nil, fmt.Errorf("scenario %s: %w", name, err)
	}
	return sc, nil
}

// splitTitle takes the first "# " line as the title; the rest is the body.
func splitTitle(md string) (title, body string) {
	lines := strings.Split(md, "\n")
	for i, l := range lines {
		if t, ok := strings.CutPrefix(l, "# "); ok {
			return strings.TrimSpace(t), strings.TrimSpace(strings.Join(slices.Delete(lines, i, i+1), "\n")) + "\n"
		}
	}
	return "", strings.TrimSpace(md) + "\n"
}

// Levels are the overlap values a scenario may expect, from forecast.Level.
var Levels = []string{forecast.None.String(), forecast.Low.String(), forecast.Medium.String(), forecast.High.String(), forecast.Critical.String()}

func (sc *Scenario) validate() error {
	var errs []error
	if !slices.Contains(Levels, sc.Overlap) {
		errs = append(errs, fmt.Errorf("overlap %q: want one of %v", sc.Overlap, Levels))
	}
	if len(sc.Tickets) == 0 {
		errs = append(errs, errors.New("no tickets"))
	}
	if sc.SafeParallelism < 1 || sc.SafeParallelism > len(sc.Tickets) {
		errs = append(errs, fmt.Errorf("safe_parallelism %d: want 1 to %d", sc.SafeParallelism, len(sc.Tickets)))
	}
	if sc.Concurrency < 0 {
		errs = append(errs, fmt.Errorf("concurrency %d is negative", sc.Concurrency))
	}
	for _, t := range sc.Tickets {
		if t.Title == "" {
			errs = append(errs, fmt.Errorf("ticket %s (%s) has no '# title' line", t.Key(), t.File))
		}
		if len(t.Edits) == 0 {
			errs = append(errs, fmt.Errorf("ticket %s has no edits", t.Key()))
		}
		for _, w := range t.WaitFor {
			if w < 1 || w > len(sc.Tickets) || w == t.Number {
				errs = append(errs, fmt.Errorf("ticket %s waits for #%d, which is not another ticket", t.Key(), w))
			}
		}
		for j, e := range t.Edits {
			if err := e.validate(); err != nil {
				errs = append(errs, fmt.Errorf("ticket %s edit %d: %w", t.Key(), j+1, err))
			}
		}
	}
	return errors.Join(errs...)
}

func (e Edit) validate() error {
	if e.Path == "" || filepath.IsAbs(e.Path) || strings.HasPrefix(filepath.Clean(e.Path), "..") {
		return fmt.Errorf("path %q must be relative to the repo", e.Path)
	}
	kinds := 0
	for _, on := range []bool{e.Old != "", e.Append, e.From != "", e.Remove} {
		if on {
			kinds++
		}
	}
	switch {
	case kinds > 1:
		return errors.New("set only one of old, append, from and remove")
	case (e.From != "" || e.Remove) && e.New != "":
		return errors.New("from and remove take no new")
	case kinds == 0 && e.New == "":
		return errors.New("nothing to do: set new, old+new, append+new, from or remove")
	}
	return nil
}

// Concurrency is autopilot's cap for the scenario.
func (sc *Scenario) cap() int {
	if sc.Concurrency > 0 {
		return sc.Concurrency
	}
	return len(sc.Tickets)
}
