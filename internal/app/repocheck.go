package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrMergeCommitsAllowed means the GitHub repo lets PRs land as merge
// commits. Saddle's stack tooling (restack, prs, the sentinel) assumes PRs
// are squash-merged and history stays linear, so saddle up, prs and land
// refuse to run until the setting is off.
//
// Rebase-merging is not refused: it also keeps history linear, and the
// stack tooling already copes with landed commits being rewritten, as it
// does for squash.
var ErrMergeCommitsAllowed = errors.New("repo allows merge commits")

// RepoGH runs gh with args in the repo and returns its stdout.
type RepoGH func(args ...string) (string, error)

// MergeSettings are the PR merge methods a GitHub repo allows.
type MergeSettings struct {
	Repo   string // owner/name
	URL    string // https://github.com/owner/name
	Merge  bool   // allow_merge_commit
	Squash bool   // allow_squash_merge
	Rebase bool   // allow_rebase_merge
}

// ReadMergeSettings reads the repo's merge settings with
// `gh api repos/{owner}/{repo}`. GitHub omits the allow_* fields for users
// without admin or push rights; that is an error, not "all false".
func ReadMergeSettings(gh RepoGH) (MergeSettings, error) {
	out, err := gh("api", "repos/{owner}/{repo}")
	if err != nil {
		return MergeSettings{}, err
	}
	var r struct {
		FullName string `json:"full_name"`
		HTMLURL  string `json:"html_url"`
		Merge    *bool  `json:"allow_merge_commit"`
		Squash   *bool  `json:"allow_squash_merge"`
		Rebase   *bool  `json:"allow_rebase_merge"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return MergeSettings{}, fmt.Errorf("parse gh api output: %w", err)
	}
	if r.Merge == nil || r.Squash == nil || r.Rebase == nil {
		return MergeSettings{}, errors.New("gh api did not return the allow_*_merge fields (they need admin or push access)")
	}
	return MergeSettings{Repo: r.FullName, URL: r.HTMLURL, Merge: *r.Merge, Squash: *r.Squash, Rebase: *r.Rebase}, nil
}

// CheckRepoMergeSettings refuses with ErrMergeCommitsAllowed when the repo
// allows merge commits. It never changes the setting. When the settings
// can't be read it writes a warning to warn and returns nil.
func CheckRepoMergeSettings(gh RepoGH, warn io.Writer) error {
	s, err := ReadMergeSettings(gh)
	if err != nil {
		fmt.Fprintf(warn, "warning: could not read the repo's merge settings (%v). "+
			"Saddle assumes PRs are squash-merged; make sure allow_merge_commit is off.\n", err)
		return nil
	}
	if !s.Merge {
		return nil
	}
	repo, settings := s.Repo, s.URL+"/settings"
	if repo == "" {
		repo = "{owner}/{repo}"
	}
	if s.URL == "" {
		settings = "the repo's GitHub settings"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s allows merge commits. Saddle stacks PRs and needs linear history: "+
		"merge commits break restack, prs and the stack sentinel.\n", repo)
	b.WriteString("Turn merge commits off, then run saddle again:\n")
	fmt.Fprintf(&b, "  - in %s: Settings -> General -> Pull Requests, uncheck \"Allow merge commits\"", settings)
	if !s.Squash {
		b.WriteString(" (and check \"Allow squash merging\")")
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "  - or: gh api -X PATCH repos/%s -F allow_merge_commit=false", repo)
	if !s.Squash {
		b.WriteString(" -F allow_squash_merge=true")
	}
	b.WriteString("\nSaddle will not change this setting for you.")
	return mergeCommitsError(b.String())
}

// mergeCommitsError is the full refusal; it matches ErrMergeCommitsAllowed.
type mergeCommitsError string

func (e mergeCommitsError) Error() string        { return string(e) }
func (e mergeCommitsError) Is(target error) bool { return target == ErrMergeCommitsAllowed }

// CheckMergeSettings runs CheckRepoMergeSettings with the gh CLI in the repo root.
func (a *App) CheckMergeSettings(warn io.Writer) error {
	return CheckRepoMergeSettings(func(args ...string) (string, error) { return gh(a.Root, args...) }, warn)
}
