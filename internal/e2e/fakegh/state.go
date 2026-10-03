// Package fakegh is a stateful stand-in for GitHub, reached through a `gh`
// shim on PATH (see cmd/gh). The repo's bare origin is "GitHub": PR state
// lives in a JSON file inside it, merges rewrite its branches with git
// plumbing, and tests read and flip that state through Repo.
//
// It implements the subset of gh saddle runs: pr create/edit/view/list/
// checks/merge/comment/close, label create, issue create/view, run view,
// repo view, auth status and a few REST and GraphQL api calls.
package fakegh

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// StateFile is the state's file name inside the bare origin.
const StateFile = "fakegh.json"

// Check states a test can set on a PR.
const (
	Pass    = "pass"
	Fail    = "fail"
	Pending = "pending"
)

// State is everything the fake GitHub knows about one repo.
type State struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
	// Default is the default branch.
	Default string   `json:"default"`
	Repo    Settings `json:"settings"`
	// Protected makes the branch protection API answer for Default.
	Protected bool `json:"protected"`
	// DefaultChecks are given to every new PR, so tests can hold CI pending.
	DefaultChecks []Check    `json:"default_checks,omitempty"`
	Next          int        `json:"next"`
	PRs           []*PR      `json:"prs"`
	Issues        []*Issue   `json:"issues"`
	Labels        []string   `json:"labels"`
	Calls         [][]string `json:"calls"`
	// Fail makes the next calls whose args start with a key fail with its value.
	Fail map[string]string `json:"fail,omitempty"`
	// NextComment numbers comment node ids.
	NextComment int `json:"next_comment"`
}

// Settings are the repo's merge settings, as the REST API names them.
type Settings struct {
	AllowMerge  bool `json:"allow_merge_commit"`
	AllowSquash bool `json:"allow_squash_merge"`
	AllowRebase bool `json:"allow_rebase_merge"`
	// DeleteBranch deletes a PR's head branch when it merges and retargets
	// PRs based on it, as GitHub's "automatically delete head branches" does.
	DeleteBranch bool `json:"delete_branch_on_merge"`
}

// PR is one pull request.
type PR struct {
	Number   int       `json:"number"`
	Title    string    `json:"title"`
	Body     string    `json:"body"`
	Head     string    `json:"head"`
	Base     string    `json:"base"`
	State    string    `json:"state"` // OPEN, CLOSED or MERGED
	Draft    bool      `json:"draft"`
	Labels   []string  `json:"labels"`
	Comments []Comment `json:"comments"`
	Checks   []Check   `json:"checks"`
	// Mergeable and MergeState override what is computed from git.
	Mergeable  string `json:"mergeable,omitempty"`
	MergeState string `json:"merge_state,omitempty"`
	// HeadOid is the head when the PR closed or merged; open PRs read the branch.
	HeadOid     string `json:"head_oid,omitempty"`
	MergeCommit string `json:"merge_commit,omitempty"`
	MergedBy    string `json:"merged_by,omitempty"` // the method
}

// Check is one CI check on a PR.
type Check struct {
	Name     string `json:"name"`
	Workflow string `json:"workflow,omitempty"`
	State    string `json:"state"` // Pass, Fail or Pending
	// Log is what `gh run view --log-failed` prints for it.
	Log string `json:"log,omitempty"`
}

// Comment is a PR comment.
type Comment struct {
	ID   string `json:"id"`
	Body string `json:"body"`
}

// Issue is one issue.
type Issue struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	State  string   `json:"state"`
	Labels []string `json:"labels"`
	Subs   []int    `json:"subs,omitempty"`
}

// Repo is the fake GitHub of one bare origin.
type Repo struct{ Dir string }

// Init writes a fresh state for the bare repo at dir: owner/name, default
// branch main, squash merges only.
func Init(dir, owner, name string) (Repo, error) {
	r := Repo{Dir: dir}
	s := State{Owner: owner, Name: name, Default: "main", Next: 1,
		Repo: Settings{AllowSquash: true, AllowRebase: true}}
	return r, r.save(&s)
}

func (r Repo) path() string { return filepath.Join(r.Dir, StateFile) }

// URL is the repo's web URL.
func (s *State) URL() string { return "https://github.com/" + s.Owner + "/" + s.Name }

// PRURL is the web URL of PR n.
func (s *State) PRURL(n int) string { return fmt.Sprintf("%s/pull/%d", s.URL(), n) }

// lock holds an exclusive lock on the state until the returned func runs.
func (r Repo) lock() (func(), error) {
	f, err := os.OpenFile(filepath.Join(r.Dir, StateFile+".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

func (r Repo) load() (*State, error) {
	b, err := os.ReadFile(r.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s is not a fake GitHub repo (no %s)", r.Dir, StateFile)
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", r.path(), err)
	}
	return &s, nil
}

func (r Repo) save(s *State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, r.path())
}

// Update runs fn on the state under the lock and saves it unless fn fails.
func (r Repo) Update(fn func(*State) error) error {
	unlock, err := r.lock()
	if err != nil {
		return err
	}
	defer unlock()
	s, err := r.load()
	if err != nil {
		return err
	}
	if err := fn(s); err != nil {
		return err
	}
	return r.save(s)
}

// Load returns a snapshot of the state.
func (r Repo) Load() (*State, error) {
	unlock, err := r.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	return r.load()
}

// PR returns the PR numbered n.
func (s *State) PR(n int) *PR {
	for _, p := range s.PRs {
		if p.Number == n {
			return p
		}
	}
	return nil
}

// Open returns the open PRs.
func (s *State) Open() []*PR {
	var out []*PR
	for _, p := range s.PRs {
		if p.State == "OPEN" {
			out = append(out, p)
		}
	}
	return out
}

// SetChecks replaces PR n's checks.
func (r Repo) SetChecks(n int, cs ...Check) error {
	return r.Update(func(s *State) error {
		p := s.PR(n)
		if p == nil {
			return fmt.Errorf("no PR #%d", n)
		}
		p.Checks = cs
		return nil
	})
}

// SetAllChecks sets one check named "ci" to state on every open PR.
func (r Repo) SetAllChecks(state string) error {
	return r.Update(func(s *State) error {
		for _, p := range s.Open() {
			p.Checks = []Check{{Name: "ci", Workflow: "CI", State: state}}
		}
		return nil
	})
}

// MergeByHand merges PR n the way a person in the GitHub UI would: no head
// check, method squash, rebase or merge.
func (r Repo) MergeByHand(n int, method string) error {
	return r.Update(func(s *State) error {
		p := s.PR(n)
		if p == nil {
			return fmt.Errorf("no PR #%d", n)
		}
		s.Calls = append(s.Calls, []string{"<ui>", "merge", fmt.Sprint(n), method})
		return merge(r.Dir, s, p, method, "")
	})
}
