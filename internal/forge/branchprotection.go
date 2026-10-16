package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// Protection is how a branch is protected. A read fills what its provider
// reports; the rest stays zero.
type Protection struct {
	Branch    string
	Protected bool // false: anyone with push access can change it
	// RequireCI means red CI can't merge: GitHub requires status checks,
	// GitLab's project says pipelines must succeed.
	RequireCI bool
	// Checks names the status checks GitHub requires; GitLab has none.
	Checks []string
	// Approvals is how many approving reviews a merge needs, -1 when the
	// forge didn't say (GitLab Free has no approval rules).
	Approvals int
	Admins    bool     // the rules bind admins too (GitHub)
	Signed    bool     // commits must be signed (GitHub)
	ForcePush bool     // force pushes are allowed
	Push      []string // who may push, as the forge words it (GitLab)
	Merge     []string // who may merge, as the forge words it (GitLab)
}

// protector is a forge that reads branch protection.
type protector interface {
	BranchProtection(ctx context.Context, branch string) (Protection, error)
}

// ReadBranchProtection is how branch is protected on f. An unprotected
// branch is Protected false, not an error; a forge that can't tell returns
// ErrUnsupported.
func ReadBranchProtection(ctx context.Context, f Forge, branch string) (Protection, error) {
	if p, ok := f.(protector); ok {
		return p.BranchProtection(ctx, branch)
	}
	return Protection{}, fmt.Errorf("read branch protection: %w", ErrUnsupported)
}

// notFound reports whether err is the API's 404: an unprotected branch.
func notFound(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "404") || strings.Contains(msg, "not protected")
}

// BranchProtection reads `gh api repos/{owner}/{repo}/branches/B/protection`.
// It needs admin access; without it GitHub answers 403, an error.
func (g *GitHub) BranchProtection(ctx context.Context, branch string) (Protection, error) {
	out, err := g.Run(ctx, "api", "repos/{owner}/{repo}/branches/"+branch+"/protection")
	if err != nil {
		if notFound(err) {
			return Protection{Branch: branch}, nil
		}
		return Protection{}, err
	}
	var p struct {
		Checks *struct {
			Contexts []string `json:"contexts"`
			Checks   []struct {
				Context string `json:"context"`
			} `json:"checks"`
		} `json:"required_status_checks"`
		Reviews *struct {
			Count int `json:"required_approving_review_count"`
		} `json:"required_pull_request_reviews"`
		Admins     struct{ Enabled bool } `json:"enforce_admins"`
		Signatures struct{ Enabled bool } `json:"required_signatures"`
		ForcePush  struct{ Enabled bool } `json:"allow_force_pushes"`
	}
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		return Protection{}, fmt.Errorf("parse gh api output: %w", err)
	}
	pr := Protection{Branch: branch, Protected: true, Admins: p.Admins.Enabled, Signed: p.Signatures.Enabled,
		ForcePush: p.ForcePush.Enabled}
	if p.Checks != nil {
		pr.Checks = slices.Clone(p.Checks.Contexts)
		for _, c := range p.Checks.Checks {
			if !slices.Contains(pr.Checks, c.Context) {
				pr.Checks = append(pr.Checks, c.Context)
			}
		}
	}
	pr.RequireCI = len(pr.Checks) > 0
	if p.Reviews != nil {
		pr.Approvals = p.Reviews.Count
	}
	return pr, nil
}

// BranchProtection reads the protected branch, then the project for
// "Pipelines must succeed" and, where the tier has it, the approvals a
// merge needs. A branch matched only by a wildcard rule reads as
// unprotected.
func (g *GitLab) BranchProtection(ctx context.Context, branch string) (Protection, error) {
	type level struct {
		Description string `json:"access_level_description"`
	}
	var b struct {
		Push      []level `json:"push_access_levels"`
		Merge     []level `json:"merge_access_levels"`
		ForcePush bool    `json:"allow_force_push"`
	}
	if err := g.apiJSON(ctx, &b, "", "projects/:id/protected_branches/"+url.PathEscape(branch)); err != nil {
		if notFound(err) {
			return Protection{Branch: branch, Approvals: -1}, nil
		}
		return Protection{}, err
	}
	var p struct {
		PipelineMustSucceed bool `json:"only_allow_merge_if_pipeline_succeeds"`
		Approvals           *int `json:"approvals_before_merge"`
	}
	if err := g.apiJSON(ctx, &p, "", "projects/:id"); err != nil {
		return Protection{}, err
	}
	pr := Protection{Branch: branch, Protected: true, RequireCI: p.PipelineMustSucceed, Approvals: -1, ForcePush: b.ForcePush}
	if p.Approvals != nil {
		pr.Approvals = *p.Approvals
	}
	for _, l := range b.Push {
		pr.Push = append(pr.Push, l.Description)
	}
	for _, l := range b.Merge {
		pr.Merge = append(pr.Merge, l.Description)
	}
	return pr, nil
}
