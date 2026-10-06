// Package trust asks the user, once per repo, whether saddle may work in it,
// the way Claude Code asks before it opens a new folder (#215). Decisions live
// in a user-level file (~/.config/saddle/trust.json, honoring
// XDG_CONFIG_HOME), never inside the repo, keyed by the repo's canonical path
// and its origin URL: moving the repo or changing its origin asks again.
package trust

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/gitx"
)

// EnvTrust set to 1 trusts the repo for this run only, without recording
// it: the non-interactive opt-in for scripts and CI.
const EnvTrust = "SADDLE_TRUST"

var (
	// ErrDeclined is the user answering no.
	ErrDeclined = errors.New("not trusted: you declined, so saddle wrote nothing")
	// ErrUntrusted is an untrusted repo with no one to ask.
	ErrUntrusted = errors.New("saddle isn't trusted in this repo")
)

// Repo identifies a repo for trust: its main checkout, symlinks resolved,
// and its origin URL ("" when it has none).
type Repo struct {
	Path   string `json:"path"`
	Origin string `json:"origin"`
}

// Identify resolves the repo containing dir. A worktree resolves to its main
// checkout, so saddle's agents inherit the repo's decision.
func Identify(dir string) (Repo, error) {
	root, err := gitx.Root(dir)
	if err != nil {
		return Repo{}, fmt.Errorf("not in a git repo: %w", err)
	}
	return Repo{Path: canonical(root), Origin: origin(root)}, nil
}

func canonical(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		p = a
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return filepath.Clean(p)
}

func origin(root string) string {
	u, err := gitx.Run(root, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	return u
}

// Entry is one remembered decision.
type Entry struct {
	Repo
	TrustedAt time.Time `json:"trusted_at"`
}

type file struct {
	Version int     `json:"version"`
	Repos   []Entry `json:"repos"`
}

// Store is the decision file.
type Store struct{ path string }

// Default is the store at $XDG_CONFIG_HOME/saddle/trust.json, falling back
// to ~/.config.
func Default() (*Store, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(home, ".config")
	}
	return Open(filepath.Join(dir, "saddle", "trust.json")), nil
}

// Open is the store at path.
func Open(path string) *Store { return &Store{path: path} }

// Path is the decision file's location.
func (s *Store) Path() string { return s.path }

func (s *Store) load() (file, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return file{Version: 1}, nil
	}
	if err != nil {
		return file{}, err
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return file{}, fmt.Errorf("%s is corrupt (run `saddle trust` to rewrite it): %w", s.path, err)
	}
	return f, nil
}

// save writes f atomically (temp file and rename), readable only by the user.
func (s *Store) save(f file) error {
	f.Version = 1
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".trust-*.json")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after the rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// Entries lists every remembered decision.
func (s *Store) Entries() ([]Entry, error) {
	f, err := s.load()
	return f.Repos, err
}

// Trusted reports whether r, path and origin both, was trusted.
func (s *Store) Trusted(r Repo) (bool, error) {
	f, err := s.load()
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(f.Repos, func(e Entry) bool { return e.Repo == r }), nil
}

// Lookup returns the decision recorded for r's path, whatever its origin.
func (s *Store) Lookup(r Repo) (Entry, bool, error) {
	f, err := s.load()
	if err != nil {
		return Entry{}, false, err
	}
	for _, e := range f.Repos {
		if e.Path == r.Path {
			return e, true, nil
		}
	}
	return Entry{}, false, nil
}

// Remember records r as trusted, replacing any decision for its path. A
// corrupt file is rewritten rather than blocking the user.
func (s *Store) Remember(r Repo) error {
	f, err := s.load()
	if err != nil {
		f = file{}
	}
	f.Repos = slices.DeleteFunc(f.Repos, func(e Entry) bool { return e.Path == r.Path })
	f.Repos = append(f.Repos, Entry{Repo: r, TrustedAt: time.Now().UTC().Truncate(time.Second)})
	return s.save(f)
}

// Forget drops every decision for r's path.
func (s *Store) Forget(r Repo) error {
	f, err := s.load()
	if err != nil {
		f = file{}
	}
	n := len(f.Repos)
	f.Repos = slices.DeleteFunc(f.Repos, func(e Entry) bool { return e.Path == r.Path })
	if len(f.Repos) == n {
		return nil
	}
	return s.save(f)
}

// Prompt is the question, listing exactly what saddle will do in root.
func Prompt(root string) string {
	return fmt.Sprintf(`Do you trust %s?

Saddle will, in this folder:
  - create .saddle/ (its state database, config and one git worktree per agent)
  - install git hooks (reference-transaction and pre-push), chained with any hooks you already have
  - write .claude/settings.local.json in each agent worktree, with saddle's hooks and the saddle MCP server
  - run coding agents that can edit files and run commands in those worktrees
  - push branches and merge PRs on GitHub, if auto-merge is on

Only trust folders whose code and config you trust.

  1. Yes, trust this folder and continue
  2. No, exit (nothing written)
`, root)
}

// Options shape Gate.
type Options struct {
	// Store defaults to Default().
	Store *Store
	// Yes trusts and remembers without asking (--trust, saddle trust --yes).
	Yes bool
	// Interactive means a person answers on In.
	Interactive bool
	In          io.Reader
	Out         io.Writer
}

// Gate returns nil when saddle may work in dir's repo, asking first if no
// decision is remembered. Saddle's own agents (SADDLE_TASK set) run under a
// trusted parent and are never asked; SADDLE_TRUST=1 trusts this run only.
// Untrusted with no one to ask, it prints the prompt to Out and returns
// ErrUntrusted. A no returns ErrDeclined. Nothing is written unless the
// answer is yes.
func Gate(dir string, o Options) error {
	if Inherited() {
		return nil
	}
	return Decide(dir, o)
}

// Inherited reports whether this process is trusted without a recorded
// decision: one of saddle's agents, or SADDLE_TRUST=1.
func Inherited() bool {
	return os.Getenv("SADDLE_TASK") != "" || os.Getenv(EnvTrust) == "1"
}

// Decide is Gate without the env shortcuts: saddle trust uses it to record
// a decision even where Gate would let the run through.
func Decide(dir string, o Options) error {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Store == nil {
		s, err := Default()
		if err != nil {
			return err
		}
		o.Store = s
	}
	r, err := Identify(dir)
	if err != nil {
		return err
	}
	ok, err := o.Store.Trusted(r)
	if err != nil && !o.Yes && !o.Interactive {
		return err
	}
	if ok {
		return nil
	}
	if o.Yes {
		return o.Store.Remember(r)
	}
	fmt.Fprint(o.Out, Prompt(r.Path))
	if !o.Interactive {
		return fmt.Errorf("%w (%s). Run `saddle trust` in it to review and trust it, or pass --trust (or set %s=1) to trust it non-interactively", ErrUntrusted, r.Path, EnvTrust)
	}
	if o.In == nil {
		o.In = os.Stdin
	}
	fmt.Fprint(o.Out, "\nChoose 1 or 2: ")
	line, _ := bufio.NewReader(o.In).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "1", "y", "yes":
		return o.Store.Remember(r)
	}
	fmt.Fprintln(o.Out)
	return ErrDeclined
}

// State is a repo's trust status.
type State string

const (
	StateTrusted       State = "trusted"
	StateUntrusted     State = "not trusted"
	StateOriginChanged State = "origin changed"
)

// Report is what saddle trust status and saddle doctor show.
type Report struct {
	Repo     Repo
	State    State
	Recorded Entry // set unless State is StateUntrusted
	Store    string
}

// Status reports dir's repo's recorded decision.
func Status(dir string, s *Store) (Report, error) {
	if s == nil {
		var err error
		if s, err = Default(); err != nil {
			return Report{}, err
		}
	}
	r, err := Identify(dir)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Repo: r, State: StateUntrusted, Store: s.Path()}
	e, ok, err := s.Lookup(r)
	if err != nil {
		return rep, err
	}
	if ok {
		rep.Recorded = e
		rep.State = StateTrusted
		if e.Origin != r.Origin {
			rep.State = StateOriginChanged
		}
	}
	return rep, nil
}
