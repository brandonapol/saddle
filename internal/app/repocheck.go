package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/brandonapol/saddle/internal/forge"
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
type MergeSettings = forge.MergeSettings

// ReadMergeSettings reads the repo's merge settings with
// `gh api repos/{owner}/{repo}` (forge.GitHub.MergeSettings).
func ReadMergeSettings(gh RepoGH) (MergeSettings, error) {
	g := &forge.GitHub{Run: func(_ context.Context, args ...string) (string, error) { return gh(args...) }}
	return g.MergeSettings(context.Background())
}

// CheckRepoMergeSettings refuses with ErrMergeCommitsAllowed when the repo
// allows merge commits. It never changes the setting. When the settings
// can't be read it writes a warning to warn and returns nil.
func CheckRepoMergeSettings(gh RepoGH, warn io.Writer) error {
	return checkMergeSettings(func() (MergeSettings, error) { return ReadMergeSettings(gh) }, warn)
}

func checkMergeSettings(read func() (MergeSettings, error), warn io.Writer) error {
	s, err := read()
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

// CheckMergeSettings is CheckRepoMergeSettings on the repo's forge.
func (a *App) CheckMergeSettings(warn io.Writer) error {
	return checkMergeSettings(func() (MergeSettings, error) { return a.Forge().MergeSettings(context.Background()) }, warn)
}
