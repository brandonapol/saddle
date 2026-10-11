package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/brandonapol/saddle/internal/app"
)

// The panic pause (#316). `saddle remote pause` or the pause tool writes
// paused.json beside the tokens. While it exists every remote call that
// changes state is refused, on every repo and listener, and auto-merge and
// autopilot are stopped in each served repo. Reads keep working. Only
// `saddle remote resume`, run on the host, removes it and restarts what
// the pause stopped; no remote tool can.

// PauseTool is the panic button. It needs act, and no admin or confirm:
// it only takes power away, and a panic button behind a round trip is no
// panic button.
const PauseTool = "pause"

const panicFile = "paused.json"

// Panic is paused.json: who paused, and what the pause stopped in each
// repo, so resume restarts exactly that.
type Panic struct {
	By     string       `json:"by"`
	At     time.Time    `json:"at"`
	Reason string       `json:"reason,omitempty"`
	Repos  []PausedRepo `json:"repos,omitempty"`
}

// PausedRepo is what a pause stopped in one repo.
type PausedRepo struct {
	Root      string `json:"root"`
	AutoMerge bool   `json:"automerge,omitempty"` // the pause turned auto-merge off
	Autopilot bool   `json:"autopilot,omitempty"` // the pause paused autopilot
	Error     string `json:"error,omitempty"`     // stopping failed; check this repo by hand
}

// PauseState is the pause as status shows it: no repo paths.
type PauseState struct {
	By     string    `json:"by"`
	At     time.Time `json:"at,omitzero"`
	Reason string    `json:"reason,omitempty"`
}

// Pauser stops a repo's auto-merge and autopilot for a pause, and restarts
// what it stopped. AppSource implements it.
type Pauser interface {
	StopAuto() (PausedRepo, error)
	RestoreAuto(PausedRepo) error
}

func panicPath(dir string) string { return filepath.Join(dir, panicFile) }

// LoadPanic reads the pause; nil means not paused.
func LoadPanic(dir string) (*Panic, error) {
	b, err := os.ReadFile(panicPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p Panic
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", panicPath(dir), err)
	}
	return &p, nil
}

// pausedState is the pause for status and for refusals. It fails closed:
// a pause file that can't be read still pauses.
func pausedState(dir string) *PauseState {
	if dir == "" {
		return nil
	}
	p, err := LoadPanic(dir)
	switch {
	case err != nil:
		return &PauseState{By: "unknown", Reason: "the pause file can't be read: " + err.Error()}
	case p == nil:
		return nil
	}
	return &PauseState{By: p.By, At: p.At, Reason: p.Reason}
}

func pausedRefusal(p *PauseState) string {
	return fmt.Sprintf("remote control is paused (by %s): actions are refused until the owner runs saddle remote resume on the host; reads still work", p.By)
}

func (p Panic) save(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := panicPath(dir) + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, panicPath(dir))
}

// lockPanic serialises pause and resume across processes.
func lockPanic(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	l, err := os.OpenFile(filepath.Join(dir, panicFile+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(l.Fd()), syscall.LOCK_EX); err != nil {
		_ = l.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(l.Fd()), syscall.LOCK_UN)
		_ = l.Close()
	}, nil
}

// Pause pauses remote control: the pause file is written first, so actions
// are refused at once, then each repo's auto-merge and autopilot are
// stopped and recorded. Pausing while paused is safe: it stops again
// whatever was restarted, and keeps the first pause's who and when.
func Pause(dir, by, reason string, now time.Time, repos []Pauser) (Panic, error) {
	unlock, err := lockPanic(dir)
	if err != nil {
		return Panic{}, err
	}
	defer unlock()
	p := Panic{By: by, At: now, Reason: reason}
	if old, err := LoadPanic(dir); err == nil && old != nil {
		p = *old
	}
	if err := p.save(dir); err != nil {
		return p, err
	}
	var errs []error
	for _, r := range repos {
		got, err := r.StopAuto()
		if err != nil {
			got.Error = err.Error()
			errs = append(errs, fmt.Errorf("%s: %w", got.Root, err))
		}
		p.record(got)
	}
	if err := p.save(dir); err != nil {
		errs = append(errs, err)
	}
	return p, errors.Join(errs...)
}

// record merges what one stop did into the pause: a repo stopped twice
// keeps both flags.
func (p *Panic) record(r PausedRepo) {
	i := slices.IndexFunc(p.Repos, func(x PausedRepo) bool { return x.Root == r.Root })
	if i < 0 {
		p.Repos = append(p.Repos, r)
		return
	}
	p.Repos[i].AutoMerge = p.Repos[i].AutoMerge || r.AutoMerge
	p.Repos[i].Autopilot = p.Repos[i].Autopilot || r.Autopilot
	p.Repos[i].Error = r.Error
}

// Resume lifts the pause: each repo gets back what the pause stopped
// (open finds a repo's Pauser by root), and the pause file is removed even
// when a repo can't be restored, so resume always unblocks. Resuming when
// not paused is a no-op (nil Panic). An unreadable pause file is removed.
func Resume(dir string, open func(root string) (Pauser, func(), error)) (*Panic, error) {
	unlock, err := lockPanic(dir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	p, perr := LoadPanic(dir)
	if p == nil && perr == nil {
		return nil, nil
	}
	var errs []error
	if perr != nil {
		errs = append(errs, fmt.Errorf("%w; removed it, check auto-merge and autopilot by hand", perr))
		p = &Panic{}
	}
	for _, r := range p.Repos {
		if !r.AutoMerge && !r.Autopilot {
			continue
		}
		ps, done, err := open(r.Root)
		if err == nil {
			err = ps.RestoreAuto(r)
			done()
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.Root, err))
		}
	}
	if err := os.Remove(panicPath(dir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return p, err
	}
	return p, errors.Join(errs...)
}

// StopAuto turns auto-merge off and pauses autopilot, reporting which it
// changed. Auto-merge stopped after a failure is left alone, so resume
// doesn't clear that stop.
func (s AppSource) StopAuto() (PausedRepo, error) {
	a := s.A
	r := PausedRepo{Root: a.Root}
	m, err := a.AutomergeState()
	if err != nil {
		return r, err
	}
	if m.Enabled && m.Stopped == "" {
		if err := a.NewAutomerge(nil).SetEnabled(false); err != nil {
			return r, err
		}
		r.AutoMerge = true
	}
	ap, err := a.AutopilotState()
	if err != nil {
		return r, err
	}
	if ap.On && !ap.Paused {
		if _, err := a.NewAutopilot(nil).Pause(); err != nil {
			return r, err
		}
		r.Autopilot = true
	}
	return r, nil
}

// RestoreAuto restarts what StopAuto stopped, unless the owner already
// did, or ended the autopilot run, in the meantime.
func (s AppSource) RestoreAuto(r PausedRepo) error {
	a := s.A
	if r.AutoMerge {
		m, err := a.AutomergeState()
		if err != nil {
			return err
		}
		if !m.Enabled {
			if err := a.NewAutomerge(nil).SetEnabled(true); err != nil {
				return err
			}
		}
	}
	if r.Autopilot {
		ap, err := a.AutopilotState()
		if err != nil {
			return err
		}
		if ap.On && ap.Paused {
			if _, err := a.NewAutopilot(nil).Resume(); err != nil {
				return err
			}
		}
	}
	return nil
}

// PauseIn is the pause tool's input.
type PauseIn struct {
	Reason string `json:"reason,omitempty" jsonschema:"why, for the owner"`
}

// PauseOut is the pause tool's result.
type PauseOut struct {
	Message string     `json:"message"`
	Paused  PauseState `json:"paused"`
}

// pauseTool is the pause tool for one caller: it pauses every repo the
// listener serves, not just the one it was called on.
func pauseTool(st *serverState, c caller) toolDef {
	return toolDef{PauseTool, func(s *mcp.Server) {
		mcp.AddTool(s, &mcp.Tool{Name: PauseTool, Description: "Panic button: refuse every remote action, on every repo, and stop auto-merge and autopilot, " +
			"until the owner runs saddle remote resume on the host. Reads keep working. No remote tool can undo it."},
			func(_ context.Context, _ *mcp.CallToolRequest, in PauseIn) (*mcp.CallToolResult, PauseOut, error) {
				if st.panicDir == "" {
					return nil, PauseOut{}, errors.New("this server has no remote directory to keep the pause in; run saddle remote pause on the host")
				}
				p, err := Pause(st.panicDir, "remote:"+c.tok.Name, in.Reason, time.Now(), st.pausers)
				out := PauseOut{Paused: PauseState{By: p.By, At: p.At, Reason: p.Reason},
					Message: "remote control is paused: actions are refused and auto-merge and autopilot are stopped until the owner runs saddle remote resume on the host"}
				if err != nil {
					out.Message += "; but stopping failed somewhere: " + err.Error()
				}
				return nil, out, nil
			})
	}}
}

// pausers are the served repos that can stop auto-merge and autopilot.
func pausers(repos []ServedRepo) []Pauser {
	var out []Pauser
	for _, r := range repos {
		if p, ok := r.Src.(Pauser); ok {
			out = append(out, p)
		}
	}
	return out
}

// localPausers are the repos a local pause stops: [remote] repos, else the
// repo in the working directory. done closes what was opened.
func localPausers(open func() (*app.App, error), w func(string, ...any)) (ps []Pauser, done func()) {
	var apps []*app.App
	done = func() {
		for _, a := range apps {
			_ = a.Close()
		}
	}
	cfg, err := LoadConfig(UserConfigPath())
	if err != nil {
		w("can't read %s (%v); pausing the repo here only", UserConfigPath(), err)
	}
	roots, err := cfg.RepoList()
	if err != nil {
		w("%v; pausing the repo here only", err)
		roots = nil
	}
	if len(roots) == 0 {
		a, err := open()
		if err != nil {
			w("no repo to stop auto-merge and autopilot in (%v); remote actions are refused all the same", err)
			return nil, done
		}
		apps = append(apps, a)
		return []Pauser{AppSource{A: a}}, done
	}
	for _, root := range roots {
		a, err := app.Open(root)
		if err != nil {
			w("can't open %s to stop auto-merge and autopilot: %v", root, err)
			continue
		}
		apps = append(apps, a)
		ps = append(ps, AppSource{A: a})
	}
	return ps, done
}

func openPauser(root string) (Pauser, func(), error) {
	a, err := app.Open(root)
	if err != nil {
		return nil, nil, err
	}
	return AppSource{A: a}, func() { _ = a.Close() }, nil
}

func describePaused(r PausedRepo) string {
	var s []string
	if r.AutoMerge {
		s = append(s, "auto-merge")
	}
	if r.Autopilot {
		s = append(s, "autopilot")
	}
	if len(s) == 0 {
		return "nothing was running"
	}
	return strings.Join(s, " and ")
}

func pauseCmd(open func() (*app.App, error)) *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "pause",
		Short: "Panic pause: refuse every remote action and stop auto-merge and autopilot",
		Long: `Refuses every remote call that changes state, on every repo and listener,
and stops auto-merge and autopilot in [remote] repos (or the repo here).
Reads keep working. It lasts until 'saddle remote resume', which only works
on this machine; the remote pause tool does the same from outside.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := Dir()
			if err != nil {
				return err
			}
			errOut := cmd.ErrOrStderr()
			ps, done := localPausers(open, func(f string, a ...any) { fmt.Fprintf(errOut, "saddle remote pause: "+f+"\n", a...) })
			defer done()
			p, err := Pause(dir, "local", reason, time.Now(), ps)
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "remote control paused (by %s at %s): remote actions are refused\n", p.By, p.At.Local().Format(time.DateTime))
			for _, r := range p.Repos {
				fmt.Fprintf(out, "  %s: stopped %s\n", r.Root, describePaused(r))
			}
			fmt.Fprintln(out, "saddle remote resume undoes it")
			return err
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why, shown in status")
	return cmd
}

func resumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume",
		Short: "Lift a panic pause and restart the auto-merge and autopilot it stopped",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := Dir()
			if err != nil {
				return err
			}
			p, err := Resume(dir, openPauser)
			out := cmd.OutOrStdout()
			if p == nil && err == nil {
				fmt.Fprintln(out, "remote control isn't paused")
				return nil
			}
			fmt.Fprintln(out, "remote control resumed: remote actions are accepted again")
			for _, r := range p.Repos {
				if r.AutoMerge || r.Autopilot {
					fmt.Fprintf(out, "  %s: restarted %s\n", r.Root, describePaused(r))
				}
			}
			return err
		},
	}
}
