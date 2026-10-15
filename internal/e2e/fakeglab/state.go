// Package fakeglab is a stateful stand-in for GitLab, reached through a
// `glab` shim on PATH (see cmd/glab), as package fakegh is for GitHub. The
// repo's bare origin is "GitLab": project, merge request, pipeline and issue
// state lives in a JSON file inside it, merges rewrite its branches with git
// plumbing, and tests read and flip that state through Repo.
//
// It implements what forge.GitLab runs: `glab api` against the REST v4
// endpoints for projects, merge requests (diffs, notes, merge), pipelines
// and jobs (trace, retry), labels and issues, plus `auth status` and
// `--version`.
package fakeglab

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// StateFile is the state's file name inside the bare origin.
const StateFile = "fakeglab.json"

// Job statuses a test can set.
const (
	Success = "success"
	Failed  = "failed"
	Running = "running"
	Pending = "pending"
)

// State is everything the fake GitLab knows about one project.
type State struct {
	Path    string `json:"path"` // namespace/name
	Default string `json:"default"`
	// MergeMethod is ff, rebase_merge or merge; SquashOption never,
	// always, default_on or default_off.
	MergeMethod  string `json:"merge_method"`
	SquashOption string `json:"squash_option"`
	// PipelineMustSucceed is "Pipelines must succeed".
	PipelineMustSucceed bool `json:"pipeline_must_succeed"`
	// DefaultJobs are the jobs of every new MR's first pipeline.
	DefaultJobs []Job      `json:"default_jobs,omitempty"`
	Next        int        `json:"next"` // ids of notes, pipelines and jobs
	NextMR      int        `json:"next_mr"`
	NextIssue   int        `json:"next_issue"`
	MRs         []*MR      `json:"mrs"`
	Issues      []*Issue   `json:"issues"`
	Labels      []string   `json:"labels"`
	Calls       [][]string `json:"calls"`
	// Fail makes calls whose args, joined by spaces, start with a key fail
	// with its value.
	Fail map[string]string `json:"fail,omitempty"`
}

// MR is one merge request.
type MR struct {
	IID         int      `json:"iid"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Source      string   `json:"source"`
	Target      string   `json:"target"`
	State       string   `json:"state"` // opened, closed or merged
	Labels      []string `json:"labels"`
	Notes       []Note   `json:"notes"`
	// Pipeline is the head pipeline; nil when CI never ran.
	Pipeline *Pipeline `json:"pipeline,omitempty"`
	// AutoMerge is "merge when pipeline succeeds", set by a merge call
	// while the pipeline ran.
	AutoMerge bool   `json:"auto_merge,omitempty"`
	Squash    bool   `json:"squash,omitempty"`
	HeadSHA   string `json:"head_sha,omitempty"` // the head when it closed or merged
	MergedSHA string `json:"merged_sha,omitempty"`
}

// Pipeline is one CI pipeline and its jobs.
type Pipeline struct {
	ID   int    `json:"id"`
	Jobs []*Job `json:"jobs"`
}

// Job is one CI job.
type Job struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	Stage        string `json:"stage"`
	Status       string `json:"status"`
	AllowFailure bool   `json:"allow_failure,omitempty"`
	// Trace is the job log `jobs/:id/trace` returns.
	Trace string `json:"trace,omitempty"`
	// Retried counts pipeline retries that reran it.
	Retried int `json:"retried,omitempty"`
}

// Note is an MR or issue note.
type Note struct {
	ID     int    `json:"id"`
	Body   string `json:"body"`
	System bool   `json:"system,omitempty"`
}

// Issue is one issue.
type Issue struct {
	IID         int      `json:"iid"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	State       string   `json:"state"` // opened or closed
	Labels      []string `json:"labels"`
	Notes       []Note   `json:"notes"`
}

// Repo is the fake GitLab of one bare origin.
type Repo struct{ Dir string }

// Init writes a fresh state for the bare repo at dir: project ns/name,
// default branch main, fast-forward merges with squash on by default (the
// linear history saddle needs).
func Init(dir, ns, name string) (Repo, error) {
	r := Repo{Dir: dir}
	s := State{Path: ns + "/" + name, Default: "main", MergeMethod: "ff", SquashOption: "default_on",
		Next: 100, NextMR: 1, NextIssue: 1}
	return r, r.save(&s)
}

// URL is the project's web URL.
func (s *State) URL() string { return "https://gitlab.com/" + s.Path }

// MRURL is the web URL of MR iid.
func (s *State) MRURL(iid int) string { return fmt.Sprintf("%s/-/merge_requests/%d", s.URL(), iid) }

func (r Repo) path() string { return filepath.Join(r.Dir, StateFile) }

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
		return nil, fmt.Errorf("%s is not a fake GitLab project (no %s)", r.Dir, StateFile)
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

// MR returns the MR numbered iid.
func (s *State) MR(iid int) *MR {
	for _, m := range s.MRs {
		if m.IID == iid {
			return m
		}
	}
	return nil
}

// ByURL returns the MR at url.
func (s *State) ByURL(url string) *MR {
	for _, m := range s.MRs {
		if s.MRURL(m.IID) == url {
			return m
		}
	}
	return nil
}

// Open returns the open MRs.
func (s *State) Open() []*MR {
	var out []*MR
	for _, m := range s.MRs {
		if m.State == "opened" {
			out = append(out, m)
		}
	}
	return out
}

// UserNotes are m's notes a person (or saddle) wrote.
func (m *MR) UserNotes() []Note {
	var out []Note
	for _, n := range m.Notes {
		if !n.System {
			out = append(out, n)
		}
	}
	return out
}

// newPipeline starts a pipeline with copies of jobs.
func (s *State) newPipeline(jobs []Job) *Pipeline {
	s.Next++
	p := &Pipeline{ID: s.Next}
	for _, j := range jobs {
		s.Next++
		j.ID = s.Next
		p.Jobs = append(p.Jobs, &j)
	}
	return p
}

// status is the pipeline's overall status: failed when a job that may not
// fail did, running while any job hasn't finished, else success.
func (p *Pipeline) status() string {
	st := Success
	for _, j := range p.Jobs {
		switch j.Status {
		case Failed:
			if !j.AllowFailure {
				return Failed
			}
		case Success, "skipped", "manual", "canceled":
		default:
			st = Running
		}
	}
	return st
}

// SetJobs gives MR iid a new head pipeline with jobs, as a push would. An
// MR set to merge when its pipeline succeeds merges once it has.
func (r Repo) SetJobs(iid int, jobs ...Job) error {
	return r.Update(func(s *State) error {
		m := s.MR(iid)
		if m == nil {
			return fmt.Errorf("no MR !%d", iid)
		}
		m.Pipeline = s.newPipeline(jobs)
		return s.autoMerge(r.Dir, m)
	})
}

// SetAllJobs gives every open MR one "test" job in status.
func (r Repo) SetAllJobs(status string) error {
	return r.Update(func(s *State) error {
		for _, m := range s.Open() {
			m.Pipeline = s.newPipeline([]Job{{Name: "test", Stage: "test", Status: status}})
			if err := s.autoMerge(r.Dir, m); err != nil {
				return err
			}
		}
		return nil
	})
}

// autoMerge merges m when it is set to merge on a pipeline that passed.
func (s *State) autoMerge(dir string, m *MR) error {
	if !m.AutoMerge || m.State != "opened" || m.Pipeline == nil || m.Pipeline.status() != Success {
		return nil
	}
	m.AutoMerge = false
	return merge(dir, s, m, m.Squash, "")
}

// MergeByHand merges MR iid the way a person in the GitLab UI would.
func (r Repo) MergeByHand(iid int) error {
	return r.Update(func(s *State) error {
		m := s.MR(iid)
		if m == nil {
			return fmt.Errorf("no MR !%d", iid)
		}
		s.Calls = append(s.Calls, []string{"<ui>", "merge", fmt.Sprint(iid)})
		return merge(r.Dir, s, m, s.SquashOption != "never", "")
	})
}
