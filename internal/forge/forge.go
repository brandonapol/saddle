// Package forge is saddle's one interface over its git host: GitHub through
// gh today, GitLab through glab next (#645). It is the only package that
// starts a forge CLI. See docs/adr/0001-forge-layer.md.
package forge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/brandonapol/saddle/internal/config"
)

// ErrUnsupported is what an operation returns when the selected forge can't
// do it. Callers degrade the feature instead of failing.
var ErrUnsupported = errors.New("not supported by this forge")

// PR states and mergeability, as saddle has always spelled them; a provider
// maps its own onto these.
const (
	StateOpen   = "OPEN"
	StateClosed = "CLOSED"
	StateMerged = "MERGED"

	Mergeable   = "MERGEABLE"
	Conflicting = "CONFLICTING"
	Unknown     = "UNKNOWN"
)

// PR is a pull request (a merge request on GitLab). A read fills only the
// fields it was asked for.
type PR struct {
	Number     int
	URL        string
	Title      string
	State      string // StateOpen, StateClosed or StateMerged
	Draft      bool
	Mergeable  string // Mergeable, Conflicting or Unknown
	MergeState string // CLEAN, BLOCKED, BEHIND, DIRTY, UNSTABLE, UNKNOWN…
	Base       string // the branch it targets, or was merged into
	Head       string // its head branch
	HeadSHA    string // its head commit
	Body       string
	Labels     []string
	Files      []string // the paths it changes
	Checks     []CheckRun
}

// PRFields picks what a PR read asks for.
type PRFields uint

const (
	PRState PRFields = 1 << iota
	PRMergeable
	PRBase
	PRHead
	PRBody
	PRNumber
	PRURL
	PRTitle
	PRDraft
	PRMergeState
	PRHeadBranch
	PRLabels
	PRFiles
	PRChecks
)

// Kinds of CheckRun.
const (
	KindJob    = "job"    // a CI job (a check run on GitHub)
	KindStatus = "status" // a commit status set by an outside service
)

// CheckRun is one entry of a PR's check rollup: a CI job or a commit status.
// Each watcher folds these into its own verdict, so they carry the raw
// states.
type CheckRun struct {
	Kind       string // KindJob, KindStatus, or "" when the forge didn't say
	Name       string // a job's name
	Workflow   string // a job's workflow (pipeline)
	Context    string // a commit status's name
	Status     string // a job's progress: QUEUED, IN_PROGRESS, COMPLETED…
	Conclusion string // a finished job's result: SUCCESS, FAILURE, SKIPPED…
	State      string // a commit status's state: SUCCESS, PENDING, FAILURE, ERROR, EXPECTED
	StartedAt  string // RFC 3339
}

// CICheck is one CI check on a PR as its current state.
type CICheck struct {
	Name     string
	Workflow string
	Bucket   string // pass, fail, pending, skipping or cancel
	Link     string // its details page; for a CI job, the job page
}

// JobLog is the end of a failed CI job's log.
type JobLog struct {
	Run  string // the run (pipeline) page the job belongs to
	Step string // the step the log ends in, when known
	Tail string // the last lines, as plain text
}

// NewPR is a PR to open.
type NewPR struct {
	Base, Head, Title, Body string
	Draft                   bool
}

// PRQuery picks PRs to list.
type PRQuery struct {
	State string // open, closed, merged or all
	Head  string // only PRs from this head branch; "" is any
	Limit int
}

// Comment is a comment on a PR. ID is what EditComment takes.
type Comment struct {
	ID   string
	Body string
}

// Issue is an issue. A read fills only the fields it was asked for.
type Issue struct {
	Number  int
	Title   string
	State   string // OPEN or CLOSED
	Body    string
	URL     string
	Labels  []string
	Created time.Time
}

// IssueFields picks what an issue read asks for.
type IssueFields uint

const (
	IssueNumber IssueFields = 1 << iota
	IssueTitle
	IssueState
	IssueBody
	IssueURL
	IssueLabels
	IssueCreated
)

// IssueQuery picks issues to list.
type IssueQuery struct {
	Label string
	State string // open, closed or all
	Limit int
}

// NewIssue is an issue to file. Its labels must exist (EnsureLabel).
type NewIssue struct {
	Repo        string // "owner/name"; "" is this repo
	Title, Body string
	Labels      []string
}

// Label is a label to make sure exists.
type Label struct {
	Name, Color, Description string
}

// MergeSettings are the merge methods the repo allows.
type MergeSettings struct {
	Repo   string // owner/name
	URL    string // the repo's web page
	Merge  bool   // merge commits
	Squash bool
	Rebase bool
}

// Forge is a git host. Core operations are needed to run stacked PRs; the
// rest may return ErrUnsupported. The ADR's capability matrix says which.
// Every ref is a PR's URL or number.
type Forge interface {
	// Provider is config.ForgeGitHub or config.ForgeGitLab.
	Provider() string

	// CreatePR opens p and returns its URL.
	CreatePR(ctx context.Context, p NewPR) (string, error)
	PR(ctx context.Context, ref string, f PRFields) (PR, error)
	ListPRs(ctx context.Context, q PRQuery, f PRFields) ([]PR, error)
	SetPRBase(ctx context.Context, ref, base string) error
	SetPRBody(ctx context.Context, ref, body string) error
	ClosePR(ctx context.Context, ref string) error
	Comments(ctx context.Context, ref string) ([]Comment, error)
	AddComment(ctx context.Context, ref, body string) error
	EditComment(ctx context.Context, id, body string) error
	AddLabel(ctx context.Context, ref, label string) error
	RemoveLabel(ctx context.Context, ref, label string) error
	EnsureLabel(ctx context.Context, l Label) error
	// MergePR merges ref with method (squash, rebase or merge), only while
	// its head commit is still head when head isn't "". It never bypasses
	// branch protection.
	MergePR(ctx context.Context, ref, method, head string) error

	// Checks lists the CI checks on ref, a PR or a branch with one. None is
	// not an error.
	Checks(ctx context.Context, ref string) ([]CICheck, error)
	// FailedLog fetches the last lines of the failed log behind a check's
	// link. A link that isn't a CI job's comes back as the run, with no log.
	FailedLog(ctx context.Context, link string, lines int) (JobLog, error)
	// RerunFailed reruns the failed jobs of CI run id.
	RerunFailed(ctx context.Context, id string) error
	MergeSettings(ctx context.Context) (MergeSettings, error)
	AuthStatus(ctx context.Context) error

	// Issue reads issue n of repo ("owner/name"; "" is this repo).
	Issue(ctx context.Context, repo string, n int, f IssueFields) (Issue, error)
	SubIssues(ctx context.Context, repo string, n int) ([]Issue, error)
	// SubIssueNumbers lists the numbers of every sub-issue of n.
	SubIssueNumbers(ctx context.Context, repo string, n int) ([]int, error)
	// AddSubIssue links issue child under parent.
	AddSubIssue(ctx context.Context, repo string, parent, child int) error
	ListIssues(ctx context.Context, q IssueQuery, f IssueFields) ([]Issue, error)
	// CreateIssue files i and returns it with its number and URL.
	CreateIssue(ctx context.Context, i NewIssue) (Issue, error)
	CommentIssue(ctx context.Context, n int, body string) error
	EditIssueBody(ctx context.Context, n int, body string) error
	CloseIssue(ctx context.Context, n int, comment string) error
	ReopenIssue(ctx context.Context, n int) error
}

// New is the forge fc selects for the repo at dir: GitHub through gh unless
// [forge] names GitLab, glab or none, which run without one until #645.
// It starts no process.
func New(fc config.Forge, dir string) Forge {
	switch {
	case fc.CLI == config.ForgeNone:
		return Unsupported{Name: fc.Provider, Reason: `[forge] cli = "none": saddle runs local-only`}
	case fc.Provider == config.ForgeGitLab || fc.CLI == config.ForgeGlab:
		return Unsupported{Name: config.ForgeGitLab, Reason: "GitLab support is not available yet (#645)"}
	}
	return &GitHub{Run: CLI(dir, GH)}
}

// Unsupported is a forge that does nothing: every operation returns an error
// that matches ErrUnsupported and says why.
type Unsupported struct {
	Name   string // the provider, "" when none
	Reason string
}

func (u Unsupported) err(op string) error {
	return fmt.Errorf("%s: %w (%s)", op, ErrUnsupported, u.Reason)
}

func (u Unsupported) Provider() string { return u.Name }
func (u Unsupported) CreatePR(context.Context, NewPR) (string, error) {
	return "", u.err("create PR")
}
func (u Unsupported) PR(context.Context, string, PRFields) (PR, error) {
	return PR{}, u.err("view PR")
}
func (u Unsupported) ListPRs(context.Context, PRQuery, PRFields) ([]PR, error) {
	return nil, u.err("list PRs")
}
func (u Unsupported) SetPRBase(context.Context, string, string) error { return u.err("retarget PR") }
func (u Unsupported) SetPRBody(context.Context, string, string) error { return u.err("edit PR") }
func (u Unsupported) ClosePR(context.Context, string) error           { return u.err("close PR") }
func (u Unsupported) Comments(context.Context, string) ([]Comment, error) {
	return nil, u.err("read comments")
}
func (u Unsupported) AddComment(context.Context, string, string) error  { return u.err("comment") }
func (u Unsupported) EditComment(context.Context, string, string) error { return u.err("edit comment") }
func (u Unsupported) AddLabel(context.Context, string, string) error    { return u.err("label PR") }
func (u Unsupported) RemoveLabel(context.Context, string, string) error { return u.err("unlabel PR") }
func (u Unsupported) EnsureLabel(context.Context, Label) error          { return u.err("create label") }
func (u Unsupported) MergePR(context.Context, string, string, string) error {
	return u.err("merge PR")
}
func (u Unsupported) Checks(context.Context, string) ([]CICheck, error) {
	return nil, u.err("read CI checks")
}
func (u Unsupported) FailedLog(context.Context, string, int) (JobLog, error) {
	return JobLog{}, u.err("read CI log")
}
func (u Unsupported) RerunFailed(context.Context, string) error { return u.err("rerun CI") }
func (u Unsupported) MergeSettings(context.Context) (MergeSettings, error) {
	return MergeSettings{}, u.err("read merge settings")
}
func (u Unsupported) AuthStatus(context.Context) error { return u.err("auth status") }
func (u Unsupported) Issue(context.Context, string, int, IssueFields) (Issue, error) {
	return Issue{}, u.err("view issue")
}
func (u Unsupported) SubIssues(context.Context, string, int) ([]Issue, error) {
	return nil, u.err("list sub-issues")
}
func (u Unsupported) SubIssueNumbers(context.Context, string, int) ([]int, error) {
	return nil, u.err("list sub-issues")
}
func (u Unsupported) AddSubIssue(context.Context, string, int, int) error {
	return u.err("link sub-issue")
}
func (u Unsupported) ListIssues(context.Context, IssueQuery, IssueFields) ([]Issue, error) {
	return nil, u.err("list issues")
}
func (u Unsupported) CreateIssue(context.Context, NewIssue) (Issue, error) {
	return Issue{}, u.err("create issue")
}
func (u Unsupported) CommentIssue(context.Context, int, string) error {
	return u.err("comment on issue")
}
func (u Unsupported) EditIssueBody(context.Context, int, string) error { return u.err("edit issue") }
func (u Unsupported) CloseIssue(context.Context, int, string) error    { return u.err("close issue") }
func (u Unsupported) ReopenIssue(context.Context, int) error           { return u.err("reopen issue") }
